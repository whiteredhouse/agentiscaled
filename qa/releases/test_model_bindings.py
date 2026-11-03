"""Model bindings must select the real parametrized baseline test."""
from pathlib import Path
import unittest

import yaml

from inventory import load_catalog, required_pairs


class ReleaseModelBindings(unittest.TestCase):
    def test_each_native_binding_selects_the_reviewed_baseline(self):
        model = yaml.safe_load((Path(__file__).resolve().parents[2] / 'spec/exacly/releases.yaml').read_text())
        pairs = required_pairs(load_catalog())
        requirements = [r for r in model['records'] if r['type'] == 'Requirement']
        self.assertEqual(len(requirements), 4)
        for record in requirements:
            with self.subTest(record=record['id']):
                cases = record['observe']['tests']['cases']
                self.assertIn('qa/releases/test_native_acceptance.py::test_native_release_acceptance*', cases)
                for before, after in pairs:
                    pattern = ('qa/releases/test_native_acceptance.py::test_native_release_acceptance[[]' +
                               before['version'] + '-to-' + after['version'] + ']')
                    self.assertIn(pattern, cases)
