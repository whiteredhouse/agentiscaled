"""Release discovery must retain the reviewed baseline and complete coverage."""
import copy
import unittest

from discover import BASE
from test_native_acceptance import validated_pairs


class AcceptancePlan(unittest.TestCase):
    def test_empty_plan_cannot_establish_acceptance(self):
        with self.assertRaisesRegex(ValueError, "empty"):
            validated_pairs({"include": []})

    def test_later_pair_does_not_replace_the_reviewed_baseline(self):
        pins = copy.deepcopy(BASE)
        pins["releases"][0]["version"] = "9.0.0"
        pins["releases"][1]["version"] = "9.0.1"
        with self.assertRaisesRegex(ValueError, "baseline"):
            validated_pairs({"include": [{"pins": pins}]})

    def test_pair_budget_fails_instead_of_truncating(self):
        with self.assertRaisesRegex(ValueError, "budget"):
            validated_pairs({"include": [{"pins": BASE}] * 41})

    def test_baseline_versions_with_different_source_pins_are_not_the_baseline(self):
        pins = copy.deepcopy(BASE)
        pins["releases"][0]["commit"] = "0" * 40
        with self.assertRaisesRegex(ValueError, "baseline"):
            validated_pairs({"include": [{"pins": pins}]})

    def test_baseline_pair_is_preserved(self):
        entry = {"pins": BASE}
        self.assertEqual(validated_pairs({"include": [entry]}), [entry])
