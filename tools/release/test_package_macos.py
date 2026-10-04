"""Offline package fixtures; Apple tools are finite controlled boundaries."""

import hashlib
import json
import shutil
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from tools.release.package_macos import assemble


class PackageTests(unittest.TestCase):

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.runtime = self.root / 'runtime'
        for name, data in {
            'payload/runtime/arm64/llama-server': b'runtime',
            'payload/licenses/llama.cpp.txt': b'llama notice',
            'payload/licenses/third-party.txt': b'runtime notices\n'
        }.items():
            p = self.runtime / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_bytes(data)
            p.chmod(0o755 if 'server' in name else 0o644)
        self.cli = self.root / 'cli'
        self.helper = self.root / 'helper'
        self.cli.write_bytes(b'CLI')
        self.helper.write_bytes(b'helper')
        self.cli.chmod(0o755)
        self.helper.chmod(0o755)
        self.output = self.root / 'output'
        self.bundle = self.root / 'notices'
        self.bundle.mkdir()
        (self.bundle / 'notices.txt').write_text('complete notice fixture\n')
        self.metadata = {
            'GoVersion': 'go1.26.8',
            'Path': 'fixture',
            'Main': {'Path': 'fixture'},
            'Deps': [],
            'Settings': [{'Key': 'GOOS', 'Value': 'darwin'}, {'Key': 'GOARCH', 'Value': 'arm64'}]
        }
        self.record = {
            'schema': 1,
            'architecture': 'arm64',
            'runtime_version': '0.5.0',
            'source_commit': '7fe450e19305b828c199d602c23a8337aaa1f03b',
            'build_os': 'fixture',
            'deployment_target': '26.4'
        }
        (self.runtime / 'build-record.json').write_text(json.dumps(self.record))
        files = []
        for p in sorted((self.runtime / 'payload').rglob('*')):
            if p.is_file():
                files.append({
                    'path': p.relative_to(self.runtime / 'payload').as_posix(),
                    'sha256': self.digest(p),
                    'size_bytes': p.stat().st_size,
                    'purpose': 'runtime' if p.name == 'llama-server' else 'license'
                })
        (self.runtime / 'runtime-inventory.json').write_text(json.dumps({
            'schema': 1,
            'architecture': 'arm64',
            'runtime_version': '0.5.0',
            'runtime_commit': self.record['source_commit'],
            'backend': 'metal',
            'files': files
        }))
        self.write_bundle()

    def digest(self, p):
        return hashlib.sha256(p.read_bytes()).hexdigest()

    def write_bundle(self):
        (self.bundle / 'index.json').write_text(json.dumps({
            'schema': 1,
            'binaries': {
                name: {'sha256': self.digest(p), 'build_info': self.metadata} for name,
                p in [('cli', self.cli), ('helper', self.helper)]
            },
            'notices_sha256': self.digest(self.bundle / 'notices.txt'),
            'source_identity': 'fixture-source',
            'components': [
                {'path': 'Go', 'version': 'go1.26.8'},
                {'path': 'Mica Mesh', 'version': 'fixture-source'}
            ]
        }))

    def run_tool(self, args, **kwargs):
        args = [str(a) for a in args]
        if len(args) > 1 and args[1] == 'build-info':
            return json.dumps(self.metadata)
        if '-archs' in args:
            return 'arm64'
        if '-L' in args:
            return 'binary:\n\t/usr/lib/libSystem.B.dylib (compatibility version 1.0.0, current version 1.0.0)\n'
        if '-l' in args:
            return 'Section\n sectname __ggml_metallib\n segname __DATA\n size 0x7\n'
        if args[0] == '/usr/bin/codesign' and '--sign' in args:
            p = Path(args[-1])
            p.write_bytes(p.read_bytes() + b'signed')
        if args[0] == '/usr/bin/pkgbuild':
            Path(args[-1]).write_bytes(b'package fixture')
        return ''

    def call(self, **extra):
        # Native tool output and payload bytes are arm64 fixtures on either Mac.
        with patch('tools.release.package_macos.platform.system', return_value='Darwin'), \
                patch('tools.release.package_macos.platform.machine', return_value='arm64'):
            return assemble(
                self.cli,
                self.helper,
                self.runtime,
                self.bundle,
                self.output,
                '0.1.0',
                run=self.run_tool,
                **extra
            )

    def test_complete_fixture_package_preserves_runtime_notices_and_marks_unsigned(self):
        self.call(unsigned_fixture=True)
        payload = self.output / 'scripts/release'
        self.assertTrue((payload / 'release.json').is_file(), 'assembly must create a complete release')
        manifest = json.loads((payload / 'release.json').read_text())
        self.assertEqual(len(manifest['files']), 4)
        self.assertEqual(
            (payload / 'licenses/third-party.txt').read_bytes(),
            b'runtime notices\n\ncomplete notice fixture\n'
        )
        record = json.loads((self.output / 'package-record.json').read_text())
        self.assertEqual(record['status'], 'unsigned-fixture')
        self.assertFalse(record['distribution_ready'])

    def test_hashes_describe_signed_bytes(self):
        self.call(application_identity='fixture-app', installer_identity='fixture-installer')
        payload = self.output / 'scripts/release'
        manifest = json.loads((payload / 'release.json').read_text())
        for entry in manifest['files']:
            self.assertEqual(entry['sha256'], self.digest(payload / entry['path']))
        self.assertEqual((payload / 'bin/mica-mesh').read_bytes(), b'CLIsigned')

    def test_changed_inputs_are_rejected_before_signing_or_packaging(self):
        inputs = [self.cli, self.helper] + list(
            path for path in (self.runtime / 'payload').rglob('*') if path.is_file()
        )
        run_tool = self.run_tool
        for target in inputs:
            with self.subTest(input=target.relative_to(self.root)):
                original = target.read_bytes()
                effects = []

                def mutate_after_inspection(args, **kwargs):
                    result = run_tool(args, **kwargs)
                    if '-l' in args and Path(args[-1]) == self.helper:
                        target.write_bytes(original + b'changed after validation')
                    if str(args[0]) in ('/usr/bin/codesign', '/usr/bin/pkgbuild'):
                        effects.append(str(args[0]))
                    return result

                try:
                    with patch.object(self, 'run_tool', mutate_after_inspection):
                        with self.assertRaises(ValueError):
                            self.call(application_identity='fixture-app', installer_identity='fixture-installer')
                    self.assertEqual(effects, [])
                    self.assertFalse(self.output.exists())
                finally:
                    target.write_bytes(original)
                    if self.output.exists():
                        shutil.rmtree(self.output)

    def test_evidence_preserves_original_verified_bytes(self):
        index = (self.bundle / 'index.json').read_bytes()
        record = (self.runtime / 'build-record.json').read_bytes()
        run_tool = self.run_tool

        def replace_evidence_during_packaging(args, **kwargs):
            result = run_tool(args, **kwargs)
            if str(args[0]) == '/usr/bin/pkgbuild':
                (self.bundle / 'index.json').write_bytes(b'changed index')
                (self.runtime / 'build-record.json').write_bytes(b'changed record')
            return result

        with patch.object(self, 'run_tool', replace_evidence_during_packaging):
            self.call(unsigned_fixture=True)
        self.assertEqual((self.output / 'notice-index.json').read_bytes(), index)
        self.assertEqual((self.output / 'runtime-build-record.json').read_bytes(), record)
        assembled = json.loads((self.output / 'package-record.json').read_text())
        self.assertEqual(assembled['notice_index_sha256'], hashlib.sha256(index).hexdigest())

    def test_rejects_missing_notice_and_stale_binary_identity(self):
        (self.bundle / 'notices.txt').unlink()
        with self.assertRaises((ValueError, OSError)):
            self.call(unsigned_fixture=True)
        self.assertFalse(self.output.exists())
        (self.bundle / 'notices.txt').write_text('complete notice fixture\n')
        self.cli.write_bytes(b'different')
        with self.assertRaises(ValueError):
            self.call(unsigned_fixture=True)
        self.assertFalse(self.output.exists())

    def test_rejects_race_build(self):
        self.metadata['Settings'].append({'Key': '-race', 'Value': 'true'})
        self.write_bundle()
        with self.assertRaises(ValueError):
            self.call(unsigned_fixture=True)
        self.assertFalse(self.output.exists())

    def test_unrelated_output_and_incomplete_runtime_are_preserved(self):
        self.output.mkdir()
        keep = self.output / 'keep'
        keep.write_text('preserve')
        with self.assertRaises(ValueError):
            self.call(unsigned_fixture=True)
        self.assertEqual(keep.read_text(), 'preserve')
        keep.unlink()
        self.output.rmdir()
        (self.runtime / 'payload/licenses/llama.cpp.txt').unlink()
        with self.assertRaises((ValueError, OSError)):
            self.call(unsigned_fixture=True)
        self.assertFalse(self.output.exists())

if __name__ == '__main__':
    unittest.main()
