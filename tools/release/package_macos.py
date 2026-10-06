"""Assemble a native macOS installer from explicit Bazel and runtime outputs.

An unsigned fixture package is never a distribution artifact. Signed output
still needs notarization, stapling, a final checksum, and native acceptance.
"""

import argparse
import hashlib
import json
from pathlib import Path
import platform
import re
import shutil
import stat
import subprocess
import sys

from tools.release.prepare_runtime import (
    COMMIT, RUNTIME_VERSION, CommandCleanupError, inspect_native, inventory,
    run_command, system_path, write_json,
)


def read_json(path, limit=65536):
    return parse_json(regular_bytes(Path(path), limit))


def parse_json(data):

    def pairs(items):
        result = {}
        for key, value in items:
            if key in result:
                raise ValueError('Duplicate JSON field: ' + key)
            result[key] = value
        return result
    return json.loads(data, object_pairs_hook=pairs)


def regular_bytes(path, limit):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size <= 0 or (info.st_size > limit):
        raise ValueError('Expected a nonempty, bounded regular file: ' + str(path))
    with path.open('rb') as stream:
        data = stream.read(limit + 1)
    if len(data) > limit:
        raise ValueError('Input grew beyond its bound: ' + str(path))
    return data


def digest(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size <= 0:
        raise ValueError('Expected a nonempty regular file: ' + str(path))
    result = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(65536), b''):
            result.update(chunk)
    return result.hexdigest()


def build_info(helper, binary, run):
    value = json.loads(run([helper, 'build-info', binary]))
    settings = {item['Key']: item['Value'] for item in value['Settings']}
    if settings.get('-race') == 'true' or settings.get('-msan') == 'true' or settings.get('-asan') == 'true':
        raise ValueError('Instrumented binaries are not release inputs. Build without race or sanitizer instrumentation.')
    if settings.get('GOOS') != 'darwin' or settings.get('GOARCH') not in ('arm64', 'amd64'):
        raise ValueError('The CLI and helper must be native macOS Bazel outputs.')
    if 'boringcrypto' in settings.get('GOEXPERIMENT', ''):
        raise ValueError('BoringCrypto needs a separate notice review; use the standard release configuration.')
    if not value.get('GoVersion', '').startswith('go'):
        raise ValueError('Go SDK identity is missing.')
    for module in value.get('Deps') or []:
        if module.get('Replace'):
            raise ValueError('Module replacements need a separate source review: ' + module['Path'])
    return (value, settings['GOARCH'])


def check_notice_bundle(bundle, cli, helper, run):
    index_bytes = regular_bytes(bundle / 'index.json', 4 * 1024 * 1024)
    index = parse_json(index_bytes)
    if index.get('schema') != 1 or set(index.get('binaries', {})) != {'cli', 'helper'}:
        raise ValueError('Unsupported notice bundle. Prepare it again from these binaries.')
    architecture = None
    for name, binary in (('cli', cli), ('helper', helper)):
        actual, arch = build_info(helper, binary, run)
        record = index['binaries'][name]
        if record != {'sha256': digest(binary), 'build_info': actual}:
            raise ValueError('Notice bundle does not match the ' + name + '. Prepare notices from the final Bazel output.')
        if architecture is not None and arch != architecture:
            raise ValueError('CLI and installer architecture differ.')
        architecture = arch
    text = regular_bytes(bundle / 'notices.txt', 16 * 1024 * 1024)
    if not text.strip() or hashlib.sha256(text).hexdigest() != index.get('notices_sha256'):
        raise ValueError('Notice text is missing or changed. Prepare the notice bundle again.')
    components = {(c['path'], c['version']) for c in index['components']}
    if len(components) != len(index['components']):
        raise ValueError('Duplicate notice component identity.')
    required = set()
    for record in index['binaries'].values():
        info = record['build_info']
        required.add(('Go', info['GoVersion']))
        required.update(((dep['Path'], dep['Version']) for dep in info.get('Deps') or []))
    required.add(('Mica Mesh', index.get('source_identity', '')))
    archive_only = {
        (c['path'], c['version']) for c in index['components']
        if c.get('scope') == 'link-input-only'
    }
    archive_inputs = {
        (module['path'], module['version'])
        for inputs in index.get('link_inputs', {}).values()
        for module in inputs['modules']
    }
    if components - archive_only != required or archive_only != archive_inputs - required:
        raise ValueError('Notice component coverage differs from the binaries. Prepare a complete bundle.')
    return (text, architecture, index, index_bytes)


def assembly_inventory(payload, architecture, run):
    expected = {
        'bin/mica-mesh': 'cli',
        'runtime/' + architecture + '/llama-server': 'runtime',
        'licenses/llama.cpp.txt': 'license',
        'licenses/third-party.txt': 'license'
    }
    entries = []
    for name, purpose in sorted(expected.items()):
        path = payload / name
        entries.append({
            'path': name,
            'purpose': purpose,
            'size_bytes': path.stat().st_size,
            'sha256': digest(path)
        })
    return entries


def assemble(
    cli,
    helper,
    runtime,
    bundle,
    output,
    version,
    *,
    unsigned_fixture=False,
    application_identity=None,
    installer_identity=None,
    run=run_command
):
    cli, helper, runtime, bundle, output = [Path(p).absolute() for p in (
        cli,
        helper,
        runtime,
        bundle,
        output
    )]
    if output.exists() or output.is_symlink():
        raise ValueError('Output already exists. Preserve it and select a fresh output directory.')
    if not re.fullmatch('[A-Za-z0-9][A-Za-z0-9._-]*', version) or len(version) > 128:
        raise ValueError('Use a product version of at most 128 letters, digits, dots, underscores, or hyphens.')
    if unsigned_fixture and (application_identity or installer_identity):
        raise ValueError('Do not combine unsigned fixture mode and signing identities.')
    if not unsigned_fixture and (not (application_identity and installer_identity)):
        raise ValueError('Supply both Developer ID identities, or explicitly select unsigned fixture mode.')
    notices, architecture, notice_index, notice_index_bytes = check_notice_bundle(bundle, cli, helper, run)
    native = 'arm64' if platform.machine() == 'arm64' else 'amd64' if platform.machine() == 'x86_64' else 'unsupported'
    if platform.system() != 'Darwin' or architecture != native:
        raise ValueError('Assemble on the matching native Mac.')
    if run(['/usr/sbin/sysctl', '-in', 'sysctl.proc_translated']).strip() == '1':
        raise ValueError('Rosetta is unsupported. Use a native terminal.')
    original = read_json(runtime / 'runtime-inventory.json')
    record_bytes = regular_bytes(runtime / 'build-record.json', 65536)
    record = parse_json(record_bytes)
    backend = 'metal' if architecture == 'arm64' else 'cpu'
    expected = {
        'schema': 1,
        'architecture': architecture,
        'runtime_version': RUNTIME_VERSION,
        'runtime_commit': COMMIT,
        'backend': backend
    }
    if set(original) != set(expected) | {'files'} or any((original.get(k) != v for k, v in expected.items())):
        raise ValueError('Runtime inventory identity differs from the pinned release.')
    if any(
        record.get(k) != v for k, v in {
            'schema': 1,
            'architecture': architecture,
            'runtime_version': RUNTIME_VERSION,
            'source_commit': COMMIT
        }.items()
    ) or not record.get('build_os') or not record.get('deployment_target'):
        raise ValueError('Native runtime build evidence is incomplete or mismatched.')
    payload_input = runtime / 'payload'
    actual = inventory(payload_input, architecture, lambda binary: inspect_native(binary, run))
    if actual != original['files']:
        raise ValueError('Runtime preparation bytes changed. Prepare the pinned runtime again.')
    for entry in actual:
        # Check the original preparation files for hard links before copying.
        digest(payload_input / entry['path'])
    for binary in (cli, helper):
        info = inspect_native(binary, run)
        if info['architecture'] != ('arm64' if architecture == 'arm64' else 'x86_64'):
            raise ValueError('Native executable architecture differs from its build metadata.')
        if any((not system_path(path) for path in info['libraries'] + info['rpaths'])):
            raise ValueError('Native executable depends on a non-system path. Rebuild through the pinned release toolchain.')
        if not binary.stat().st_mode & 0o111:
            raise ValueError('Bazel executable is not executable: ' + str(binary))
    output.mkdir()
    try:
        scripts = output / 'scripts'
        scripts.mkdir()
        payload = scripts / 'release'
        shutil.copytree(payload_input, payload)
        # Bind staged bytes to the verified input identities before changing
        # notices or signatures. A new manifest alone cannot prove this link.
        staged = inventory(payload, architecture, lambda binary: inspect_native(binary, run))
        if staged != original['files']:
            raise ValueError('Runtime input changed during staging. Prepare the pinned runtime again.')
        (payload / 'bin').mkdir()
        shutil.copyfile(cli, payload / 'bin/mica-mesh')
        shutil.copyfile(helper, scripts / 'installer')
        for name, binary in (('cli', payload / 'bin/mica-mesh'), ('helper', scripts / 'installer')):
            if digest(binary) != notice_index['binaries'][name]['sha256']:
                raise ValueError('The ' + name + ' changed during staging. Prepare notices from stable Bazel outputs.')
        notice_path = payload / 'licenses/third-party.txt'
        notice_path.write_bytes(notice_path.read_bytes() + b'\n' + notices)
        binaries = [
            payload / 'runtime' / architecture / 'llama-server',
            payload / 'bin/mica-mesh',
            scripts / 'installer'
        ]
        for binary in binaries:
            binary.chmod(0o755)
            if not unsigned_fixture:
                run([
                    '/usr/bin/codesign',
                    '--force',
                    '--sign',
                    application_identity,
                    '--timestamp',
                    '--options',
                    'runtime',
                    binary
                ])
                run(['/usr/bin/codesign', '--verify', '--strict', '--verbose=2', binary])
        manifest = {
            **expected,
            'version': version,
            'tested_os': [record['build_os']],
            'files': assembly_inventory(payload, architecture, run)
        }
        write_json(payload / 'release.json', manifest)
        manifest_digest = digest(payload / 'release.json')
        run([scripts / 'installer', 'verify', payload, manifest_digest])
        for phase in ('preinstall', 'postinstall'):
            hook = (
                '#!/bin/sh\nset -eu\n'
                'assets=$(CDPATH= cd -- "$(dirname "$0")" && pwd)\n'
                'exec "$assets/installer" ' + phase + ' "$assets/release" "'
                + manifest_digest + '" "$3"\n'
            )
            (scripts / phase).write_text(hook)
            (scripts / phase).chmod(0o755)
        suffix = '-UNSIGNED-FIXTURE' if unsigned_fixture else ''
        package = output / ('mica-mesh-' + version + '-darwin-' + architecture + suffix + '.pkg')
        command = [
            '/usr/bin/pkgbuild',
            '--nopayload',
            '--scripts',
            scripts,
            '--identifier',
            'org.mica-mesh.installer',
            '--version',
            version
        ]
        if not unsigned_fixture:
            command += ['--sign', installer_identity, '--timestamp']
        run(command + [package])
        if not unsigned_fixture:
            run(['/usr/sbin/pkgutil', '--check-signature', package])
        package_digest = digest(package)
        (output / (package.name + '.sha256')).write_text(package_digest + '  ' + package.name + '\n')
        write_json(
            output / 'package-record.json',
            {
                'schema': 1,
                'version': version,
                'architecture': architecture,
                'status': 'unsigned-fixture' if unsigned_fixture else 'signed-awaiting-notarization',
                'distribution_ready': False,
                'package': package.name,
                'package_sha256': package_digest,
                'checksum_stage': 'assembled-before-notarization-and-stapling',
                'release_sha256': manifest_digest,
                'notice_index_sha256': hashlib.sha256(notice_index_bytes).hexdigest(),
                'source_identity': notice_index['source_identity'],
                'runtime_source_commit': COMMIT,
                'runtime_build_os': record['build_os'],
                'deployment_target': record['deployment_target'],
                'native_acceptance': 'pending',
                'notarization': 'pending',
                'stapling': 'pending'
            }
        )
        (output / 'notice-index.json').write_bytes(notice_index_bytes)
        (output / 'runtime-build-record.json').write_bytes(record_bytes)
        return package
    except CommandCleanupError:
        # Preserve files while an owned Apple-tool process may still use them.
        raise
    except BaseException:
        shutil.rmtree(output)
        raise


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ('cli', 'helper', 'runtime', 'bundle', 'output'):
        parser.add_argument('--' + name, type=Path, required=True)
    parser.add_argument('--version', required=True)
    parser.add_argument('--unsigned-fixture', action='store_true')
    parser.add_argument('--application-identity')
    parser.add_argument('--installer-identity')
    args = parser.parse_args()
    try:
        package = assemble(**vars(args))
    except (OSError, ValueError, subprocess.SubprocessError) as error:
        print('Package assembly failed: ' + str(error), file=sys.stderr)
        return 1
    print('Package assembled: ' + str(package))
    print('Distribution is pending notarization, stapling, final checksum, and native acceptance.')
    return 0

if __name__ == '__main__':
    sys.exit(main())
