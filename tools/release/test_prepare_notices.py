"""Fixture coverage for offline notices and explicit build identities."""

import json
from pathlib import Path
import tempfile
import unittest

from tools.release.prepare_notices import prepare_notices


class NoticeTests(unittest.TestCase):

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.cli = self.root / 'cli'
        self.cli.write_bytes(b'cli')
        self.helper = self.root / 'helper'
        self.helper.write_bytes(b'helper')
        self.metadata = {
            'GoVersion': 'go1.26.8',
            'Path': 'github.com/AbhinavSingh95/mica-mesh/cmd/mica-mesh',
            'Main': {'Path': 'github.com/AbhinavSingh95/mica-mesh'},
            'Deps': [{'Path': 'github.com/google/uuid', 'Version': 'v1.6.0'}],
            'Settings': [{'Key': 'GOOS', 'Value': 'darwin'}, {'Key': 'GOARCH', 'Value': 'arm64'}]
        }
        self.source = self.root / 'module'
        self.source.mkdir()
        (self.source / 'LICENSE').write_text('full original module notice\n')
        self.purl = self.root / 'metadata.json'
        self.purl.write_text(json.dumps({'purl': 'pkg:golang/github.com/google/uuid@v1.6.0'}))
        self.sdk = self.root / 'sdk'
        self.sdk.mkdir()
        (self.sdk / 'VERSION').write_text('go1.26.8\n')
        for name in [
            'LICENSE',
            'PATENTS'
        ] + ['src/vendor/golang.org/x/' + m + '/' + n for m in ('crypto', 'net', 'sys', 'text') for n in ('LICENSE', 'PATENTS')]:
            p = self.sdk / name
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text('full SDK notice ' + name + '\n')
        (self.sdk / 'src/math').mkdir(parents=True)
        (self.sdk / 'src/math/exp.go').write_text(
            'package math\n// Copyright Sun Microsystems\n'
            '// Full permission and disclaimer fixture.\nfunc F() {}\n'
        )
        (self.sdk / 'src/math/atan.go').write_text(
            'package math\n// Copyright Stephen L. Moshier\n'
            '// Full permission and disclaimer fixture.\nfunc G() {}\n'
        )
        self.project = self.root / 'project'
        self.project.mkdir()
        (self.project / 'LICENSE').write_text('full project notice\n')
        (self.project / 'main.go').write_text('package main\n')
        self.params = self.root / 'link.params'
        self.params.write_text(
            "link\n-arc\n'@@repo//:uuid=github.com/google/uuid=archive.a'\n"
            "-package_metadata\n'github.com/google/uuid=uuid.metadata.json'\n"
        )
        self.config = {
            'schema': 1,
            'source_identity': 'fixture-checkpoint',
            'project_root': str(self.project),
            'project_files': ['LICENSE', 'main.go'],
            'sdk_root': str(self.sdk),
            'modules': [{'root': str(self.source), 'metadata': str(self.purl), 'patches': []}],
            'link_params': {'cli': str(self.params), 'helper': str(self.params)},
            'metadata_inputs': {'uuid.metadata.json': str(self.purl)},
        }
        self.inputs = self.root / 'sources.json'
        self.inputs.write_text(json.dumps(self.config))
        self.output = self.root / 'bundle'

    def call(self):
        self.inputs.write_text(json.dumps(self.config))
        return prepare_notices(
            self.cli,
            self.helper,
            self.inputs,
            self.output,
            run=lambda args: json.dumps(self.metadata)
        )

    def test_complete_sources_produce_notice_text_and_binary_bound_index(self):
        self.call()
        self.assertTrue((self.output / 'notices.txt').is_file(), 'must export offline original notices')
        text = (self.output / 'notices.txt').read_text()
        for original in (
            'full original module notice',
            'full project notice',
            'Full permission and disclaimer fixture.',
            'UNICODE LICENSE V3'
        ):
            self.assertIn(original, text)
        index = json.loads((self.output / 'index.json').read_text())
        self.assertEqual(
            {(c['path'], c['version']) for c in index['components']},
            {('Go', 'go1.26.8'), ('Mica Mesh', 'fixture-checkpoint'), ('github.com/google/uuid', 'v1.6.0')}
        )
        self.assertEqual(set(index['binaries']), {'cli', 'helper'})
        self.assertNotIn('func F()', text)

    def test_missing_notice_version_mismatch_and_uncovered_helper_fail(self):
        for failure in ('empty', 'version', 'helper'):
            with self.subTest(failure=failure):
                if failure == 'empty':
                    (self.source / 'LICENSE').write_text('')
                elif failure == 'version':
                    self.purl.write_text(json.dumps({'purl': 'pkg:golang/github.com/google/uuid@v9.9.9'}))
                else:
                    self.metadata['Deps'].append({'Path': 'golang.org/x/sys', 'Version': 'v0.47.0'})
                with self.assertRaises((ValueError, OSError)):
                    self.call()
                self.assertFalse(self.output.exists())
                (self.source / 'LICENSE').write_text('full original module notice\n')
                self.purl.write_text(json.dumps({'purl': 'pkg:golang/github.com/google/uuid@v1.6.0'}))

    def test_linked_conditional_port_requires_review(self):
        with self.params.open('a') as stream:
            stream.write(
                "-arc\n'@@repo//:layout=github.com/charmbracelet/ultraviolet/internal/casso=archive.a'\n"
                "-package_metadata\n'github.com/charmbracelet/ultraviolet/internal/casso=uv.metadata.json'\n"
            )
        metadata = self.root / 'uv.metadata.json'
        metadata.write_text(json.dumps({'purl': 'pkg:golang/github.com/charmbracelet/ultraviolet@v1.0.0'}))
        self.config['metadata_inputs']['uv.metadata.json'] = str(metadata)
        with self.assertRaisesRegex(ValueError, 'casso'):
            self.call()
        self.assertFalse(self.output.exists())

    def test_archive_only_notice_is_preserved_without_claiming_binary_linkage(self):
        source = self.root / 'legacy'
        source.mkdir()
        (source / 'LICENSE').write_text('full legacy notice\n')
        metadata = self.root / 'legacy.metadata.json'
        metadata.write_text(json.dumps({'purl': 'pkg:golang/github.com/golang/protobuf@v1.5.4'}))
        with self.params.open('a') as stream:
            stream.write(
                "-arc\n'@@unused//:old=github.com/golang/protobuf/proto=unused.a'\n"
                "-package_metadata\n'github.com/golang/protobuf/proto=legacy.metadata.json'\n"
            )
        self.config['metadata_inputs']['legacy.metadata.json'] = str(metadata)
        self.config['modules'].append({'root': str(source), 'metadata': str(metadata), 'patches': []})
        self.call()
        index = json.loads((self.output / 'index.json').read_text())
        component = next(c for c in index['components'] if c['path'] == 'github.com/golang/protobuf')
        self.assertEqual(component['scope'], 'link-input-only')
        self.assertIn('full legacy notice', (self.output / 'notices.txt').read_text())

    def test_unresolved_archive_fails(self):
        with self.params.open('a') as stream:
            stream.write("-arc\n'@@unused//:old=example.invalid/unused=unused.a'\n")
        with self.assertRaisesRegex(ValueError, 'metadata'):
            self.call()

    def test_race_and_unmatched_sdk_are_rejected(self):
        self.metadata['Settings'].append({'Key': '-race', 'Value': 'true'})
        with self.assertRaises(ValueError):
            self.call()
        self.metadata['Settings'].pop()
        (self.sdk / 'VERSION').write_text('go9.0.0\n')
        with self.assertRaises(ValueError):
            self.call()
        self.assertFalse(self.output.exists())

if __name__ == '__main__':
    unittest.main()
