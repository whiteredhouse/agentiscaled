"""Proof and disclosure boundaries for actual native UI diagnostics."""
import json
import os
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock

import preflight


def proof():
    return {
        "schema_version": 1, "controller": "native-ax", "status": "passed", "trusted": True,
        "identity_verified": True, "authentication": "unknown", "elapsed_seconds": 2.5, "barriers": [],
        "manual_accessibility": {"supported": True, "original_known": True, "enabled": True,
                                 "enable_result": "success", "restored": True, "restore_result": "success"},
        "inspection": {"elements": 6, "web_area_seen": True, "truncated": False,
                       "role_counts": {"window": 1, "web_area": 1, "button": 2, "text_field": 1,
                                       "text_area": 0, "static_text": 1, "group": 0, "other": 0,
                                       "scroll_area": 0, "menu": 0, "menu_item": 0, "menu_bar": 0, "unknown": 0},
                       "sign_in_label": False, "onboarding_label": False, "chat_label": False,
                       "owner_lookup_failures": 0, "foreign_owner_elements": 0, "owned_bundle_foreign_elements": 0},
        "native_window_control": {"status": "passed", "reason": "verified", "attempted": True,
                                  "minimize_readback": True, "restored": True},
        "renderer_control": {"status": "passed", "reason": "verified", "attempted": True,
                             "focus_readback": True, "restored": True,
                             "focus_write_result": "success", "focus_attempts": 1,
                             "restore_write_result": "success", "restore_attempts": 1},
    }


class NativeUIProofTests(unittest.TestCase):
    def assert_invalid(self, value):
        with self.assertRaisesRegex(preflight.PreflightError, "^invalid_native_ui_control_result$"):
            preflight.project_ui_control(value)

    def test_verified_control_does_not_claim_authenticated_or_desktop_acceptance(self):
        value = proof()
        value["inspection"]["chat_label"] = True
        result = preflight.project_ui_control(value)
        self.assertEqual(result["authentication"], "unknown")
        self.assertEqual(result["status"], "passed")
        report = preflight.initial_report()
        report["ui_control"] = result
        self.assertEqual((report["desktop_tool_calls"], report["acceptance"], report["release_readiness"]),
                         ("not_run", "not_run", "not_established"))

    def test_window_control_alone_cannot_establish_renderer_control(self):
        value = proof()
        value["renderer_control"].update(status="blocked", reason="no_web_area", attempted=False,
                                          focus_readback=False, restored=False,
                                          focus_write_result="not_attempted", focus_attempts=0,
                                          restore_write_result="not_attempted", restore_attempts=0)
        value["inspection"]["web_area_seen"] = False
        self.assert_invalid(value)
        value["status"] = "blocked"
        result = preflight.project_ui_control(value)
        self.assertEqual(result["native_window_control"]["status"], "passed")
        self.assertEqual(result["renderer_control"]["status"], "blocked")

    def test_setter_without_readback_or_original_restoration_cannot_pass(self):
        for section, flags in (("native_window_control", ("attempted", "minimize_readback", "restored")),
                               ("renderer_control", ("attempted", "focus_readback", "restored"))):
            for flag in flags:
                value = proof()
                value[section][flag] = False
                with self.subTest(section=section, flag=flag):
                    self.assert_invalid(value)

    def test_trust_identity_and_actual_renderer_exposure_are_required(self):
        for key in ("trusted", "identity_verified"):
            value = proof()
            value[key] = False
            self.assert_invalid(value)
        value = proof()
        value["inspection"]["web_area_seen"] = False
        self.assert_invalid(value)

    def test_custom_accessibility_mutation_requires_known_restored_original(self):
        for key in ("original_known", "restored"):
            value = proof()
            value["manual_accessibility"][key] = False
            self.assert_invalid(value)

    def test_native_numeric_boolean_is_rejected_with_only_the_fixed_field_name(self):
        value = proof()
        value["manual_accessibility"]["enabled"] = 1
        with self.assertRaises(preflight.PreflightError) as raised:
            preflight.project_ui_control(value)
        self.assertEqual(str(raised.exception), "invalid_native_ui_control_result")
        self.assertEqual(raised.exception.validation_field, "manual_accessibility_enabled")

    def test_login_classification_requires_observed_fixed_login_signal(self):
        value = proof()
        value["authentication"] = "sign_in_required"
        self.assert_invalid(value)
        value["inspection"]["sign_in_label"] = True
        self.assertEqual(preflight.project_ui_control(value)["authentication"], "sign_in_required")
        value["authentication"] = "authenticated"
        self.assert_invalid(value)

    def test_only_fixed_diagnostics_survive_projection(self):
        value = proof()
        sensitive = "must-never-export-session-material"
        value.update(raw_ui=sensitive, cookies=sensitive, password=sensitive)
        value["inspection"]["labels"] = [sensitive]
        value["renderer_control"]["value"] = sensitive
        result = preflight.project_ui_control(value)
        self.assertNotIn(sensitive, json.dumps(result))
        self.assertNotIn("raw_ui", result)
        for section, field in (("renderer_control", "reason"), ("manual_accessibility", "enable_result")):
            changed = proof()
            changed[section][field] = sensitive
            self.assert_invalid(changed)
        changed = proof()
        changed["barriers"] = [sensitive]
        self.assert_invalid(changed)

    def test_malformed_or_unbounded_report_cannot_pass(self):
        for value in (None, [], {}, {"schema_version": True}):
            self.assert_invalid(value)
        for elapsed in (True, -1, 61, float("nan"), float("inf")):
            value = proof()
            value["elapsed_seconds"] = elapsed
            self.assert_invalid(value)
        for count in (True, -1, 513, "6"):
            value = proof()
            value["inspection"]["elements"] = count
            self.assert_invalid(value)
        value = proof()
        value["inspection"]["role_counts"]["button"] = 7
        self.assert_invalid(value)
        value = proof()
        value["inspection"]["owned_bundle_foreign_elements"] = 1
        self.assert_invalid(value)

    def test_failed_action_and_restore_remain_failed(self):
        value = proof()
        value["status"] = "failed"
        value["renderer_control"].update(status="failed", reason="restore_failed", restored=False)
        value["barriers"] = ["renderer_control_failed"]
        result = preflight.project_ui_control(value)
        self.assertEqual(result["status"], "failed")
        self.assertFalse(result["renderer_control"]["restored"])

    def test_bounded_restoration_retry_requires_setter_success_and_original_readback(self):
        value = proof()
        value["renderer_control"]["restore_attempts"] = 2
        restored = preflight.project_ui_control(value)["renderer_control"]
        self.assertTrue(restored["restored"])
        self.assertEqual(restored["restore_attempts"], 2)
        for change in ({"restore_write_result": "cannot_complete"}, {"restore_attempts": 0},
                       {"restore_write_result": "not_attempted"}):
            with self.subTest(change=change):
                changed = proof()
                changed["renderer_control"].update(change)
                self.assert_invalid(changed)

    def test_bounded_selected_focus_requires_actual_target_readback_and_setter_success(self):
        value = proof()
        value["renderer_control"]["focus_attempts"] = 2
        focused = preflight.project_ui_control(value)["renderer_control"]
        self.assertTrue(focused["focus_readback"])
        self.assertEqual(focused["focus_attempts"], 2)
        for change in ({"focus_write_result": "cannot_complete"}, {"focus_attempts": 0},
                       {"focus_write_result": "not_attempted"}, {"focus_readback": False}):
            with self.subTest(change=change):
                changed = proof()
                changed["renderer_control"].update(change)
                self.assert_invalid(changed)

    def test_restored_original_does_not_promote_failed_target_focus(self):
        value = proof()
        value["status"] = "failed"
        value["renderer_control"].update(status="failed", reason="focus_readback_failed", focus_readback=False,
                                          focus_write_result="success", focus_attempts=3)
        result = preflight.project_ui_control(value)
        self.assertEqual(result["status"], "failed")
        self.assertFalse(result["renderer_control"]["focus_readback"])
        self.assertTrue(result["renderer_control"]["restored"])
        for count in (True, -1, 33, 1.0, "1"):
            changed = proof()
            changed["renderer_control"]["focus_attempts"] = count
            self.assert_invalid(changed)
        for error in ("private-native-focus-value", "unchanged", "already_enabled", []):
            changed = proof()
            changed["renderer_control"]["focus_write_result"] = error
            self.assert_invalid(changed)

    def test_failed_restore_retains_only_bounded_fixed_diagnostics(self):
        value = proof()
        value["status"] = "failed"
        value["renderer_control"].update(status="failed", reason="restore_failed", restored=False,
                                          restore_write_result="invalid_ui_element", restore_attempts=1)
        result = preflight.project_ui_control(value)["renderer_control"]
        self.assertEqual(result["restore_write_result"], "invalid_ui_element")
        self.assertFalse(result["restored"])
        for count in (True, -1, 33, 1.0, "1"):
            changed = proof()
            changed["renderer_control"]["restore_attempts"] = count
            self.assert_invalid(changed)
        for error in ("private-native-error-text", "unchanged", "already_enabled", []):
            changed = proof()
            changed["renderer_control"]["restore_write_result"] = error
            self.assert_invalid(changed)


    def test_driver_exit_and_application_identity_are_independently_checked(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            driver = root / "controller"
            driver.write_text("not executed by this test")
            driver.chmod(0o700)
            identity = {"pid": 1234, "start": [1700000000, 123], "executable": "/owned/Claude.app/Contents/MacOS/Claude"}
            completed = SimpleNamespace(returncode=0, stdout=json.dumps(proof()), stderr="private native output")
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}), mock.patch(
                    "preflight.subprocess.run", return_value=completed) as run, mock.patch(
                    "preflight.process_identity", return_value=identity), mock.patch("preflight.digest", return_value="c" * 64):
                result = preflight.probe_ui_control(driver, identity, Path("/owned/Claude.app"), {"bundle_id": "test.bundle"})
                self.assertEqual(result["status"], "passed")
                self.assertNotIn("private native output", json.dumps(result))
                self.assertEqual(run.call_args.kwargs["timeout"], 65)
                completed.returncode = 1
                with self.assertRaisesRegex(preflight.PreflightError, "exit_mismatch"):
                    preflight.probe_ui_control(driver, identity, Path("/owned/Claude.app"), {"bundle_id": "test.bundle"})
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}), mock.patch(
                    "preflight.subprocess.run", return_value=completed), mock.patch("preflight.digest", return_value="c" * 64), mock.patch(
                    "preflight.process_identity", side_effect=[identity, dict(identity, start=[1700000001, 0])]):
                with self.assertRaisesRegex(preflight.PreflightError, "probe_identity_changed"):
                    preflight.probe_ui_control(driver, identity, Path("/owned/Claude.app"), {"bundle_id": "test.bundle"})

    def test_application_exit_is_retained_as_a_fixed_failure_without_exception_text(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / "report"
            with mock.patch("sys.argv", ["preflight.py", "--pins", "unused.json", "--out", str(output),
                                         "--ui-driver", "unused-controller"]), mock.patch(
                    "preflight.preflight", side_effect=ProcessLookupError("unretained private exception")):
                self.assertEqual(preflight.main(), 1)
            report = json.loads((output / "preflight.json").read_text())
            self.assertEqual(report["error"], "owned_application_exited")
            self.assertEqual(report["status"], "failed")
            self.assertNotIn("private exception", json.dumps(report))


if __name__ == "__main__":
    unittest.main()
