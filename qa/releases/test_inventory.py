"""Release catalog audit and complete-coverage controls; all future data is synthetic."""
import copy
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import discover
from inventory import load_catalog, required_pairs, select_release, validate_catalog
from test_native_acceptance import validated_pairs


def future_catalog():
    catalog = copy.deepcopy(discover.CATALOG)
    catalog["releases"].append({"version": "0.1.12", "commit": "1" * 40,
                                 "manifest_sha256": "2" * 64, "tap_commit": "3" * 40,
                                 "status": "audited"})
    return catalog


class ReleaseCatalog(unittest.TestCase):
    def test_complete_existing_baseline_is_preserved(self):
        catalog = load_catalog()
        self.assertEqual(required_pairs(catalog), [tuple(discover.BASE["releases"])])
        self.assertEqual(select_release(catalog, "0.1.11"), discover.BASE["releases"][1])
        self.assertEqual(catalog["baseline_pair"], ["0.1.10", "0.1.11"])
        with self.assertRaisesRegex(ValueError, "baseline"):
            load_catalog(baseline={})

    def test_baseline_cannot_be_replaced_or_rewritten(self):
        for change in ("pair", "commit", "manifest", "missing"):
            with self.subTest(change=change):
                catalog = future_catalog()
                if change == "pair":
                    catalog["baseline_pair"] = ["0.1.11", "0.1.12"]
                elif change == "missing":
                    catalog["releases"] = catalog["releases"][1:]
                else:
                    field = "commit" if change == "commit" else "manifest_sha256"
                    catalog["releases"][0][field] = "0" * len(catalog["releases"][0][field])
                with self.assertRaisesRegex(ValueError, "baseline"):
                    validate_catalog(catalog, discover.BASE)

    def test_schema_order_budget_and_duplicate_versions_fail_closed(self):
        for change in ("schema", "repository", "duplicate", "order", "budget"):
            with self.subTest(change=change):
                catalog = future_catalog()
                if change == "schema":
                    catalog["schema_version"] = True
                elif change == "repository":
                    catalog["repository"] = "unrelated/repository"
                elif change == "duplicate":
                    catalog["releases"].append(copy.deepcopy(catalog["releases"][-1]))
                elif change == "order":
                    catalog["releases"].reverse()
                else:
                    catalog["releases"] *= 21
                with self.assertRaises(ValueError):
                    validate_catalog(catalog, discover.BASE)

    def test_future_audit_requires_complete_immutable_metadata(self):
        for key, value in (("commit", "main"), ("manifest_sha256", "short"), ("tap_commit", None),
                           ("version", "0.1.12-rc1"), ("license_sha256", "bad"),
                           ("status", "published"), ("unexpected", "value")):
            with self.subTest(key=key):
                catalog = future_catalog()
                catalog["releases"][-1][key] = value
                with self.assertRaises(ValueError):
                    validate_catalog(catalog, discover.BASE)
        for missing in ("commit", "manifest_sha256", "tap_commit"):
            catalog = future_catalog()
            del catalog["releases"][-1][missing]
            with self.assertRaises(ValueError):
                validate_catalog(catalog, discover.BASE)

    def test_source_selection_cannot_override_audited_commit(self):
        catalog = validate_catalog(future_catalog(), discover.BASE)
        self.assertEqual(select_release(catalog, "0.1.12", commit="1" * 40)["manifest_sha256"], "2" * 64)
        with self.assertRaisesRegex(ValueError, "source"):
            select_release(catalog, "0.1.12", commit="0" * 40)
        with self.assertRaisesRegex(ValueError, "absent"):
            select_release(catalog, "0.1.13")

    def test_pending_release_is_never_promoted_by_discovery_or_selection(self):
        catalog = future_catalog()
        catalog["releases"][-1] = {"version": "0.1.12", "commit": "1" * 40,
                                   "status": "pending-publication"}
        validate_catalog(catalog, discover.BASE)
        rows = [{"tag_name": "v" + release["version"], "draft": False, "prerelease": False}
                for release in catalog["releases"]]
        with mock.patch.object(discover, "CATALOG", catalog), mock.patch("discover.api") as api:
            for operation in (lambda: select_release(catalog, "0.1.12"), lambda: discover.pairs(rows),
                              lambda: discover.plan(False, False), lambda: discover.resolve("v0.1.12", [])):
                with self.assertRaisesRegex(ValueError, "pending publication and audit"):
                    operation()
            api.assert_not_called()
        catalog["releases"][-1]["manifest_sha256"] = "2" * 64
        with self.assertRaisesRegex(ValueError, "must not claim"):
            validate_catalog(catalog, discover.BASE)

    def test_new_release_adds_required_pair_without_dropping_baseline(self):
        catalog = validate_catalog(future_catalog(), discover.BASE)
        with mock.patch.object(discover, "CATALOG", catalog):
            plan = discover.plan(False, False)
            self.assertEqual([tuple(release["version"] for release in entry["pins"]["releases"])
                              for entry in plan["include"]], [("0.1.10", "0.1.11"), ("0.1.11", "0.1.12")])
            self.assertEqual(validated_pairs(plan), plan["include"])
            with self.assertRaisesRegex(ValueError, "catalog pair"):
                validated_pairs({"include": plan["include"][:1]})
            with self.assertRaisesRegex(ValueError, "baseline"):
                validated_pairs({"include": plan["include"][1:]})
            mutated = copy.deepcopy(plan)
            mutated["include"][1]["pins"]["releases"][1]["commit"] = "0" * 40
            with self.assertRaisesRegex(ValueError, "catalog pair"):
                validated_pairs(mutated)

    def test_automatic_plan_uses_audited_future_metadata_without_rediscovery(self):
        catalog = validate_catalog(future_catalog(), discover.BASE)
        rows = [{"tag_name": "v" + release["version"], "draft": False, "prerelease": False, "assets": []}
                for release in catalog["releases"]]
        with mock.patch.object(discover, "CATALOG", catalog), mock.patch("discover.api", return_value=rows) as api:
            plan = discover.plan(True, False)
            self.assertEqual([tuple(release["version"] for release in entry["pins"]["releases"])
                              for entry in plan["include"]], [("0.1.10", "0.1.11"), ("0.1.11", "0.1.12")])
            self.assertEqual(plan["include"][1]["pins"]["releases"][1]["manifest_sha256"], "2" * 64)
            self.assertEqual(validated_pairs(plan), plan["include"])
            # Known audited tap/source inputs need no mutable-history lookup.
            api.assert_called_once_with("repos/" + discover.REPO + "/releases?per_page=100&page=1")

    def test_release_api_cannot_omit_an_audited_intermediate_version(self):
        catalog = validate_catalog(future_catalog(), discover.BASE)
        rows = [{"tag_name": tag, "draft": False, "prerelease": False}
                for tag in ("v0.1.10", "v0.1.11", "v0.1.13")]
        with mock.patch.object(discover, "CATALOG", catalog):
            with self.assertRaisesRegex(ValueError, "omits required"):
                discover.pairs(rows)
            rows.append({"tag_name": "v0.1.12", "draft": False, "prerelease": False})
            self.assertEqual(discover.pairs(rows), [("v0.1.10", "v0.1.11"), ("v0.1.11", "v0.1.12"),
                                                   ("v0.1.12", "v0.1.13")])

    def test_discovery_unions_catalog_pairs_with_intermediate_stable_releases(self):
        catalog = future_catalog()
        catalog["releases"][-1]["version"] = "0.1.13"
        validate_catalog(catalog, discover.BASE)
        rows = [{"tag_name": tag, "draft": False, "prerelease": False}
                for tag in ("v0.1.10", "v0.1.11", "v0.1.12", "v0.1.13")]
        with mock.patch.object(discover, "CATALOG", catalog):
            self.assertEqual(discover.pairs(rows), [("v0.1.10", "v0.1.11"), ("v0.1.11", "v0.1.12"),
                                                   ("v0.1.11", "v0.1.13"), ("v0.1.12", "v0.1.13")])

    def test_catalog_files_reject_duplicate_keys_symlinks_and_oversize(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "catalog.json"
            path.write_text('{"schema_version":1,"schema_version":1}')
            with self.assertRaisesRegex(ValueError, "duplicate"):
                load_catalog(path)
            path.write_text("x" * (1024 * 1024 + 1))
            with self.assertRaisesRegex(ValueError, "budget"):
                load_catalog(path)
            link = Path(directory) / "link.json"
            link.symlink_to(path)
            with self.assertRaisesRegex(ValueError, "symlink"):
                load_catalog(link)
