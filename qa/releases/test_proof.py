import json
from pathlib import Path
import tempfile
import unittest
from proof import PLATFORMS, check_pair


class CompleteProof(unittest.TestCase):
    def fixture(self, root):
        def write(name, value):
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(json.dumps(value))
        write('pins.json', {'releases': [{'version': 'old'}, {'version': 'new'}]})
        write('release-artifacts.json', {'success': True, 'assets': [{}] * 16})
        for i in range(18):
            write(f'attestations/{i}.attestation.json', {})
        for target in PLATFORMS:
            system, arch = target.split('/')
            native = {'success': True, 'system': system, 'arch': arch,
                      'homebrew': {'operations': [{'version': 'old'}, {'version': 'new'}]}}
            if system == 'linux':
                native['packages'] = [{'format': k, 'install_upgrade': 'passed'} for k in ('deb', 'rpm')]
            write(f'release-{system}-{arch}.json', native)
            write(f'lifecycle-{system}-{arch}.json', {
                'success': True, 'system': system, 'arch': arch, 'identity_before': {'key': 'same'},
                'identity_after': {'key': 'same'}, 'cleanup': 'passed',
                'observations': [{'version': v, 'proc_exe_sha256': 'expected',
                                  'expected_release_exe_sha256': 'expected'} for v in ('old', 'old', 'new')],
                'mesh_evidence': {'enrollments': ['one']}, 'homebrew_cleanup': 'passed',
                'upgrade_method': {'manager': 'Homebrew', 'old_versioned_executable_removed': True,
                                   'operations': [{'operation': 'install', 'version': 'old'},
                                                  {'operation': 'upgrade', 'version': 'new'}]}})

    def test_missing_native_target_cannot_pass(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self.fixture(root)
            (root / 'lifecycle-darwin-arm64.json').unlink()
            with self.assertRaises(FileNotFoundError):
                check_pair(root)

    def test_replaced_path_does_not_prove_loaded_image(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self.fixture(root)
            path = root / 'lifecycle-darwin-amd64.json'
            report = json.loads(path.read_text())
            report['observations'][-1]['proc_exe_sha256'] = 'still-old'
            path.write_text(json.dumps(report))
            with self.assertRaisesRegex(ValueError, 'loaded service image'):
                check_pair(root)

    def test_complete_native_matrix_is_required(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            self.fixture(root)
            self.assertEqual(len(check_pair(root)['releases']), 2)
