"""Negative controls for headless authentication and primary-node isolation.

These helper checks cannot establish native service behavior. The native runner
still has to exercise the published node under its real service manager.
"""
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import subprocess
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest import mock

from lifecycle import check_headless_evidence, fixture_post, primary_unchanged, run, secret_absent


class PrimaryIsolation(unittest.TestCase):
    previous = {"pid": 4123, "start": 90421}

    def check(self, ready, *, preserved=None):
        current = dict(self.previous)

        def unchanged(previous):
            if current != previous:
                raise ValueError("primary process changed")

        supervisor = SimpleNamespace(unchanged_process=mock.Mock(side_effect=unchanged))

        def observe(data):
            return ready(current)

        with mock.patch("lifecycle.wait_ready", side_effect=observe):
            result = primary_unchanged(supervisor, self.previous, Path("/fixture/primary"),
                                       "primary-peer", preserved or {})
        self.assertEqual(supervisor.unchanged_process.call_count, 2)
        return result

    def test_ready_primary_retains_process_peer_and_preserved_files(self):
        with tempfile.TemporaryDirectory() as directory:
            settings = Path(directory).resolve() / "settings.json"
            settings.write_bytes(b"original settings")
            result = self.check(lambda _: {"peer_id": "primary-peer"},
                                preserved={settings: settings.read_bytes()})
        self.assertEqual(result, dict(self.previous, peer_id="primary-peer", readyz=200))

    def test_restart_during_ready_probe_is_not_primary_isolation(self):
        def ready(current):
            current["pid"] += 1
            return {"peer_id": "primary-peer"}

        with self.assertRaisesRegex(ValueError, "primary process changed"):
            self.check(ready)

    def test_ready_node_with_a_different_peer_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "primary node's peer identity"):
            self.check(lambda _: {"peer_id": "replacement-peer"})

    def test_stable_process_without_primary_readiness_cannot_establish_isolation(self):
        def not_ready(_):
            raise RuntimeError("primary node is no longer ready")

        with self.assertRaisesRegex(RuntimeError, "no longer ready"):
            self.check(not_ready)

    def test_settings_or_service_changes_during_secondary_work_are_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            paths = [Path(directory).resolve() / name for name in ("settings.json", "service.unit")]
            for changed in paths:
                for path in paths:
                    path.write_bytes(b"preserved bytes")
                preserved = {path: path.read_bytes() for path in paths}

                def ready(_):
                    changed.write_bytes(b"secondary changed primary")
                    return {"peer_id": "primary-peer"}

                with self.subTest(changed=changed.name), self.assertRaisesRegex(
                        ValueError, "primary settings or service"):
                    self.check(ready, preserved=preserved)


class HeadlessAuthenticationEvidence(unittest.TestCase):
    baseline = {"oauth_device_requests": 1, "oauth_token_requests": 1, "signin_enrollment_requests": 1}

    def evidence(self):
        return dict(self.baseline, enrollments=["primary-peer"], join_code_peers=["secondary-peer"],
                    denied_auth_requests=0, denied_join_requests=0, unexpected_requests=0,
                    rejected_join_requests=2, signin_locked=True, enrollment_locked=True,
                    join_requests=[{"peer_id": "secondary-peer", "machine_name": "qa-headless"}])

    def check(self, evidence):
        check_headless_evidence(evidence, self.baseline, "primary-peer", "secondary-peer")

    def test_one_signin_one_code_join_and_both_negative_controls_are_recognized(self):
        self.check(self.evidence())

    def test_unarmed_or_missing_authentication_guards_cannot_establish_headless_reuse(self):
        for guard in ("signin_locked", "enrollment_locked"):
            for present in (False, True):
                evidence = self.evidence()
                if present:
                    evidence[guard] = False
                else:
                    del evidence[guard]
                with self.subTest(guard=guard, present=present), self.assertRaises((KeyError, ValueError)):
                    self.check(evidence)

    def test_authentication_or_reenrollment_cannot_pass_as_headless_reuse(self):
        for key, value in (
                ("oauth_token_requests", 2), ("signin_enrollment_requests", 2),
                ("denied_auth_requests", 1),
                ("enrollments", ["primary-peer", "secondary-peer"]),
                ("join_code_peers", ["secondary-peer", "replacement-peer"])):
            evidence = self.evidence()
            evidence[key] = value
            with self.subTest(changed=key), self.assertRaises(ValueError):
                self.check(evidence)

    def test_missing_code_controls_or_wrong_join_provenance_are_rejected(self):
        for key, value in (
                ("rejected_join_requests", 1), ("join_requests", []),
                ("join_requests", [{"peer_id": "primary-peer", "machine_name": "qa-headless"}])):
            evidence = self.evidence()
            evidence[key] = value
            with self.subTest(changed=key, value=value), self.assertRaises(ValueError):
                self.check(evidence)

    def test_omitted_authentication_and_code_attempt_metadata_cannot_establish_success(self):
        for missing in ("oauth_token_requests", "join_code_peers", "rejected_join_requests", "join_requests"):
            evidence = self.evidence()
            del evidence[missing]
            with self.subTest(missing=missing), self.assertRaises((KeyError, ValueError)):
                self.check(evidence)


class JoinCodeAbsence(unittest.TestCase):
    secret = "synthetic-headless-join-code"

    def test_caller_owned_code_input_outside_node_and_home_is_allowed(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            home, data = root / "home", root / "node"
            home.mkdir()
            data.mkdir()
            (root / "join-code-input").write_text(self.secret)
            (data / "settings").write_text("non-secret settings")
            secret_absent(self.secret, (home, data), ["Joined the private network."])

    def test_code_in_product_output_is_rejected(self):
        with self.assertRaisesRegex(ValueError, "product output"):
            secret_absent(self.secret, (), ["Joined.", "debug: " + self.secret])

    def test_successful_command_stderr_is_retained_for_code_leak_detection(self):
        captured = []
        result = subprocess.CompletedProcess(
            ["/fixture/agentiscaled", "status"], 0,
            stdout="Node: fixture-node\n", stderr="debug: " + self.secret + "\n")
        with mock.patch("lifecycle.subprocess.run", return_value=result):
            self.assertEqual(run("/fixture/agentiscaled", "status", captured=captured), "Node: fixture-node")
        with self.assertRaisesRegex(ValueError, "product output"):
            secret_absent(self.secret, (), captured)

    def test_code_in_nested_home_or_node_files_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve()
            for name in ("home", "node"):
                with self.subTest(root=name):
                    retained = root / name / "nested" / "retained.bin"
                    retained.parent.mkdir(parents=True)
                    retained.write_bytes(b"\x00prefix:" + self.secret.encode() + b":suffix\x00")
                    with self.assertRaisesRegex(ValueError, "persisted in node data or fixture HOME"):
                        secret_absent(self.secret, (root / name,), [])


class FixtureJoinStatus(unittest.TestCase):
    def post(self, status, expected):
        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                self.rfile.read(int(self.headers.get("Content-Length", "0")))
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({"fixture": True}).encode())

            def log_message(self, *arguments):
                pass

        with ThreadingHTTPServer(("127.0.0.1", 0), Handler) as server:
            worker = threading.Thread(target=server.serve_forever, daemon=True)
            worker.start()
            try:
                return fixture_post(f"http://127.0.0.1:{server.server_port}", "/v1/join", expected=expected)
            finally:
                server.shutdown()
                worker.join(timeout=2)

    def test_expected_denial_is_recognized(self):
        self.assertIsNone(self.post(403, 403))

    def test_unexpected_success_or_server_failure_cannot_satisfy_negative_join_control(self):
        for status in (200, 500):
            with self.subTest(status=status), self.assertRaises(ValueError):
                self.post(status, 403)


if __name__ == "__main__":
    unittest.main()
