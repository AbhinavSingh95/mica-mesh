"""Export offline notices bound to explicit Bazel binaries and source inputs.

The input JSON names resolved module metadata/source directories, the SDK,
project files/patches, and the two actual Bazel linker parameter files. It is a
maintainer preparation input, not another dependency/version catalogue. This
tool never searches caches, resolves dependencies, or downloads license text.
"""

import argparse
import hashlib
import json
from pathlib import Path
import re
import shlex
import shutil
import sys
from urllib.parse import unquote

from tools.release.package_macos import build_info, digest, read_json, regular_bytes
from tools.release.prepare_runtime import run_command


# Reviewed coverage policy, deliberately without module versions. Selections
# come from the two binaries and Bazel-generated purls, not this table.
ROOT_NOTICES = {
    'charm.land/bubbles/v2': ['LICENSE'],
    'charm.land/bubbletea/v2': ['LICENSE'],
    'charm.land/lipgloss/v2': ['LICENSE'],
    'github.com/atotto/clipboard': ['LICENSE'],
    'github.com/charmbracelet/colorprofile': ['LICENSE'],
    'github.com/charmbracelet/ultraviolet': ['LICENSE'],
    'github.com/charmbracelet/x/ansi': ['LICENSE'],
    'github.com/charmbracelet/x/term': ['LICENSE'],
    'github.com/charmbracelet/x/termios': ['LICENSE'],
    'github.com/charmbracelet/x/windows': ['LICENSE'],
    'github.com/clipperhouse/displaywidth': ['LICENSE'],
    'github.com/clipperhouse/uax29/v2': ['LICENSE'],
    'github.com/google/uuid': ['LICENSE'],
    'github.com/golang/protobuf': ['LICENSE'],
    'github.com/libp2p/zeroconf/v2': ['LICENSE'],
    'github.com/lucasb-eyer/go-colorful': ['LICENSE'],
    'github.com/mattn/go-runewidth': ['LICENSE'],
    'github.com/miekg/dns': ['LICENSE', 'COPYRIGHT'],
    'github.com/muesli/cancelreader': ['LICENSE'],
    'github.com/rivo/uniseg': ['LICENSE.txt'],
    'github.com/xo/terminfo': ['LICENSE'],
    'golang.org/x/net': ['LICENSE', 'PATENTS'],
    'golang.org/x/sync': ['LICENSE', 'PATENTS'],
    'golang.org/x/sys': ['LICENSE', 'PATENTS'],
    'golang.org/x/text': ['LICENSE', 'PATENTS'],
    'google.golang.org/genproto/googleapis/rpc': ['LICENSE'],
    'google.golang.org/grpc': ['LICENSE', 'NOTICE.txt'],
    'google.golang.org/protobuf': ['LICENSE', 'PATENTS']
}
PATCHED = {
    'charm.land/bubbles/v2',
    'charm.land/bubbletea/v2',
    'github.com/charmbracelet/ultraviolet',
    'github.com/libp2p/zeroconf/v2'
}
ANSI_SOURCE_SHA256 = '133c221fd98b88312d41a07bcc4049572a7f79b41d641ca87e4730ede7d8d3d8'
ASSETS = Path(__file__).parent / 'notices'


def tree_identity(root):
    """Record exact supplied source bytes, including patched/generated inputs."""
    result = hashlib.sha256()
    for path in sorted(root.rglob('*')):
        if path.is_symlink():
            value = 'link:' + str(path.readlink())
        elif path.is_file():
            h = hashlib.sha256()
            with path.open('rb') as stream:
                for chunk in iter(lambda: stream.read(65536), b''):
                    h.update(chunk)
            value = h.hexdigest()
        else:
            continue
        result.update((path.relative_to(root).as_posix() + '\x00' + value + '\n').encode())
    return result.hexdigest()


def link_inputs(path, metadata_inputs):
    tokens = shlex.split(regular_bytes(path, 8 * 1024 * 1024).decode())
    packages = set()
    metadata = {}
    for index, token in enumerate(tokens):
        if token == '-arc':
            if index + 1 >= len(tokens) or len(tokens[index + 1].split('=')) != 3:
                raise ValueError('Invalid Bazel linker archive input: ' + str(path))
            packages.add(tokens[index + 1].split('=')[1])
        elif token == '-package_metadata':
            if index + 1 >= len(tokens) or '=' not in tokens[index + 1]:
                raise ValueError('Invalid Bazel package metadata input.')
            package, original = tokens[index + 1].split('=', 1)
            if package in metadata or original not in metadata_inputs:
                raise ValueError('Missing or duplicate explicit package metadata: ' + package)
            supplied = Path(metadata_inputs[original])
            purl = read_json(supplied).get('purl', '')
            if not purl.startswith('pkg:golang/') or '@' not in purl:
                raise ValueError('Invalid resolved module purl: ' + package)
            module, version = unquote(purl[len('pkg:golang/'):]).rsplit('@', 1)
            if package != module and not package.startswith(module + '/'):
                raise ValueError('Archive and module metadata identities differ: ' + package)
            metadata[package] = (module, version)
    if not packages:
        raise ValueError('Bazel linker package inputs are missing: ' + str(path))
    for package in packages:
        if not package.startswith('github.com/AbhinavSingh95/mica-mesh/') and package not in metadata:
            raise ValueError('Archive has no resolved module metadata: ' + package)
    return packages, {metadata[p] for p in packages if p in metadata}


def prepare_notices(cli, helper, sources, output, *, run=run_command):
    cli, helper, sources, output = [Path(p).absolute() for p in (cli, helper, sources, output)]
    if output.exists() or output.is_symlink():
        raise ValueError('Notice output exists. Preserve it and select a fresh directory.')
    config = read_json(sources, 4 * 1024 * 1024)
    if config.get('schema') != 1 or not config.get('source_identity') or (not config.get('project_files')):
        raise ValueError('Explicit source identity and project file inputs are required.')
    binaries = {}
    selected = {}
    archive_modules = {}
    sdk_versions = set()
    packages = set()
    links = {}
    architecture = None
    for name, binary in (('cli', cli), ('helper', helper)):
        info, arch = build_info(helper, binary, run)
        if architecture is not None and architecture != arch:
            raise ValueError('Binary architectures differ.')
        architecture = arch
        binaries[name] = {'sha256': digest(binary), 'build_info': info}
        sdk_versions.add(info['GoVersion'])
        for module in info.get('Deps') or []:
            previous = selected.setdefault(module['Path'], module['Version'])
            if previous != module['Version']:
                raise ValueError('CLI and helper select different versions: ' + module['Path'])
        params = Path(config['link_params'][name])
        linked, input_modules = link_inputs(params, config['metadata_inputs'])
        for module, version in input_modules:
            previous = archive_modules.setdefault(module, version)
            if previous != version:
                raise ValueError('Link actions select different module versions: ' + module)
        # Each binary's recorded dependencies must occur in its own action.
        for module in info.get('Deps') or []:
            if not any((p == module['Path'] or p.startswith(module['Path'] + '/') for p in linked)):
                raise ValueError('Linker package inputs do not cover ' + module['Path'])
        packages.update(linked)
        links[name] = {
            'sha256': digest(params),
            'archive_packages': sorted(linked),
            'modules': [{'path': module, 'version': version}
                        for module, version in sorted(input_modules)],
        }
    if any(
        p.startswith('github.com/charmbracelet/ultraviolet/internal/casso')
        or p == 'github.com/charmbracelet/ultraviolet/layout'
        for p in packages
    ):
        raise ValueError('The final binary links casso. Obtain and review its original notice before packaging.')
    for module, version in selected.items():
        if archive_modules.get(module) != version:
            raise ValueError('Link archive identity differs from binary metadata: ' + module)
    resolved = {}
    for entry in config['modules']:
        metadata = read_json(Path(entry['metadata']))
        purl = metadata.get('purl', '')
        if not purl.startswith('pkg:golang/') or '@' not in purl:
            raise ValueError('Use resolved Bazel package metadata, including its purl.')
        module, version = unquote(purl[len('pkg:golang/'):]).rsplit('@', 1)
        if module in resolved:
            raise ValueError('Duplicate module source: ' + module)
        resolved[module] = (version, entry)
    if set(resolved) != set(archive_modules):
        raise ValueError('Resolved sources must cover exactly the CLI and helper archive inputs.')
    sections = []
    components = []

    def notice(component, name, path, data=None):
        data = regular_bytes(path, 4 * 1024 * 1024) if data is None else data
        if not data.strip():
            raise ValueError('Empty notice: ' + str(path))
        # Keep the original bytes after a separate identification line.
        label = component['path'] + ' ' + component['version'] + ' — ' + name
        section = ('\n===== ' + label + ' =====\n\n').encode() + data + b'\n'
        sections.append(section)
        component['notices'].append({
            'file': name,
            'sha256': hashlib.sha256(data).hexdigest(),
            'size_bytes': len(data)
        })
    for module, version in sorted(archive_modules.items()):
        if module not in ROOT_NOTICES:
            raise ValueError('Review notice coverage for new module: ' + module)
        actual, entry = resolved[module]
        if actual != version:
            raise ValueError('Resolved source version differs from the binary: ' + module)
        root = Path(entry['root']).resolve()
        scope = 'binary' if module in selected else 'link-input-only'
        component = {
            'path': module,
            'version': version,
            'scope': scope,
            'source_tree_sha256': tree_identity(root),
            'metadata_sha256': digest(Path(entry['metadata'])),
            'notices': [],
            'patches': []
        }
        if scope == 'link-input-only':
            sections.append((
                '\nConservative notice for a resolved linker input: ' + module
                + '. This is not a claim of live runtime linkage.\n'
            ).encode())
        for name in ROOT_NOTICES[module]:
            notice(component, name, root / name)
        patches = entry.get('patches', [])
        if bool(patches) != (module in PATCHED):
            raise ValueError('Patch provenance differs from the reviewed module policy: ' + module)
        for path in patches:
            patch = Path(path)
            component['patches'].append({'file': patch.name, 'sha256': digest(patch)})
        if patches:
            sections.append((
                'Mica Mesh modifies this component with: '
                + ', '.join(p['file'] for p in component['patches']) + '.\n'
            ).encode())
        if module == 'github.com/charmbracelet/x/ansi':
            if digest(root / 'color.go') != ANSI_SOURCE_SHA256:
                raise ValueError('ANSI color source changed. Review the tmux notice provenance again.')
            name = 'tmux-colour-notice-549c35b06165f6ae023115eb76f83f2cbf945395.txt'
            notice(
                component,
                'tmux color derivation; pinned notice source, copied revision unknown',
                (ASSETS / name).resolve()
            )
            if 'github.com/charmbracelet/x/ansi/sixel' in packages:
                source = regular_bytes(root / 'sixel/palette_sort.go', 1024 * 1024)
                block = source.split(b'package ', 1)[0]
                if b'Go Authors' not in block:
                    raise ValueError('Sixel source attribution changed; review its original notice.')
                notice(
                    component,
                    'sixel/palette_sort.go original header',
                    root / 'sixel/palette_sort.go',
                    block
                )
        components.append(component)
    sdk = Path(config['sdk_root']).resolve()
    sdk_version = regular_bytes(sdk / 'VERSION', 4096).decode().splitlines()[0]
    if sdk_versions != {sdk_version}:
        raise ValueError('SDK source identity differs from the binaries.')
    component = {
        'path': 'Go',
        'version': sdk_version,
        'source_tree_sha256': tree_identity(sdk / 'src'),
        'notices': []
    }
    for name in ['LICENSE', 'PATENTS'] + [
        'src/vendor/golang.org/x/' + module + '/' + name
        for module in ('crypto', 'net', 'sys', 'text')
        for name in ('LICENSE', 'PATENTS')
    ]:
        notice(component, name, sdk / name)
    markers = set()
    for path in sorted((sdk / 'src/math').glob('*.go')):
        text = regular_bytes(path, 1024 * 1024).decode()
        for block in re.findall('(?m:(?://[^\\n]*(?:\\n|$))+)|/\\*.*?\\*/', text, re.DOTALL):
            found = [marker for marker in ('Sun Microsystems', 'Stephen L. Moshier') if marker in block]
            if found:
                markers.update(found)
                notice(
                    component,
                    path.relative_to(sdk).as_posix() + ' original comment',
                    path,
                    block.encode()
                )
    if markers != {'Sun Microsystems', 'Stephen L. Moshier'}:
        raise ValueError('SDK embedded math notices are incomplete. Review this SDK before packaging.')
    provenance = read_json((ASSETS / 'provenance.json').resolve(), 4 * 1024 * 1024)
    for candidate in provenance['candidates']:
        path = (ASSETS / candidate['path']).resolve()
        if digest(path) != candidate['sha256']:
            raise ValueError('Pinned offline notice source changed: ' + candidate['path'])
    for name in ('unicode-license-v3-2026.txt', 'unicode-data-source-notices.txt'):
        notice(
            component,
            'Unicode data used by SDK and terminal dependencies: ' + name,
            (ASSETS / name).resolve()
        )
    components.append(component)
    project = Path(config['project_root']).resolve()
    state = []
    for relative in sorted(config['project_files']):
        path = Path(relative)
        if path.is_absolute() or '..' in path.parts:
            raise ValueError('Project input paths must stay within the source root.')
        state.append({'file': relative, 'sha256': digest(project / path)})
    component = {'path': 'Mica Mesh', 'version': config['source_identity'], 'files': state, 'notices': []}
    notice(component, 'LICENSE', project / 'LICENSE')
    components.append(component)
    text = b''.join(sections)
    index = {
        'schema': 1,
        'source_identity': config['source_identity'],
        'binaries': binaries,
        'link_inputs': links,
        'components': components,
        'notices_sha256': hashlib.sha256(text).hexdigest(),
        'offline_provenance_sha256': digest((ASSETS / 'provenance.json').resolve()),
        'provenance_limits': provenance['uncertainty']
    }
    output.mkdir()
    try:
        (output / 'notices.txt').write_bytes(text)
        (output / 'index.json').write_text(json.dumps(index, indent=2, sort_keys=True) + '\n')
        shutil.copyfile((ASSETS / 'provenance.json').resolve(), output / 'offline-provenance.json')
        return index
    except BaseException:
        shutil.rmtree(output)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('cli', 'helper', 'sources', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    try:
        prepare_notices(**vars(parser.parse_args()))
    except (ValueError, OSError, KeyError) as error:
        print('Notice preparation failed: ' + str(error), file=sys.stderr)
        return 1
    return 0

if __name__ == '__main__':
    sys.exit(main())
