"""Approved tool entrypoints must ignore checkout-root Python replacements."""
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

import yaml

ROOT = Path(__file__).resolve().parents[2]


class IsolatedToolEntrypoints(unittest.TestCase):
    def test_actual_workflow_entrypoints_ignore_checkout_modules(self):
        prefixes = set()
        for path in (ROOT / '.github/workflows').glob('release-*.yml'):
            workflow = yaml.safe_load(path.read_text())
            for job in workflow['jobs'].values():
                for step in job['steps']:
                    for line in step.get('run', '').splitlines():
                        if line.startswith('.venv-evidence/bin/python '):
                            words = shlex.split(line.rstrip().removesuffix('\\'))
                            marker = words.index('-m')
                            prefixes.add(tuple(words[1:marker + 2]))
        action = yaml.safe_load((ROOT / '.github/actions/release-setup/action.yml').read_text())
        for step in action['runs']['steps']:
            for line in step.get('run', '').splitlines():
                if line.startswith(('python3 ', '.venv-evidence/bin/python ')):
                    words = shlex.split(line.rstrip().removesuffix('\\'))
                    marker = words.index('-m')
                    prefixes.add(tuple(words[1:marker + 2]))
        self.assertEqual({prefix[-1] for prefix in prefixes}, {'pytest', 'exacly', 'venv', 'pip'})
        with tempfile.TemporaryDirectory() as directory:
            for module in ('pytest', 'exacly', 'venv', 'pip'):
                Path(directory, module + '.py').write_text('raise SystemExit("UNAPPROVED_CHECKOUT_MODULE")\n')
            for prefix in prefixes:
                with self.subTest(prefix=prefix):
                    result = subprocess.run([sys.executable, *prefix, '--help'], cwd=directory,
                                            env={**os.environ, 'PYTHONPATH': directory},
                                            capture_output=True, text=True, timeout=30)
                    self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                    self.assertNotIn('UNAPPROVED_CHECKOUT_MODULE', result.stdout + result.stderr)
