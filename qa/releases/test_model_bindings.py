"""Model bindings must select the real parametrized baseline test."""
from fnmatch import fnmatchcase
from pathlib import Path
import unittest

import yaml


class ReleaseModelBindings(unittest.TestCase):
    def test_each_native_binding_selects_the_reviewed_baseline(self):
        model = yaml.safe_load((Path(__file__).resolve().parents[2] / 'spec/exacly/releases.yaml').read_text())
        baseline = 'qa/releases/test_native_acceptance.py::test_native_release_acceptance[0.1.10-to-0.1.11]'
        requirements = [r for r in model['records'] if r['type'] == 'Requirement']
        self.assertEqual(len(requirements), 4)
        for record in requirements:
            with self.subTest(record=record['id']):
                self.assertTrue(all(fnmatchcase(baseline, p) for p in record['observe']['tests']['cases']))
