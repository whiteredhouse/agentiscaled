"""Negative controls for automatic recovery and the product's status evidence."""
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest import mock

from lifecycle import check_status, wait_recovered


class AutomaticRecoveryTests(unittest.TestCase):
    def test_old_pid_and_transient_manager_absence_are_not_recovery(self):
        for old_start, new_start in ((90421, 90422), ([1700000000, 200], [1700000001, 300])):
            previous = {"pid": 4123, "start": old_start}
            fresh = {"pid": 4124, "start": new_start}
            # No start/restart operation is available: recovery must be observed
            # through the native manager's identity query alone.
            supervisor = SimpleNamespace(identity=mock.Mock(side_effect=[
                previous, ProcessLookupError("old process exited"),
                ValueError("service restarting"), RuntimeError("manager has no running PID"), fresh]))
            with self.subTest(start=old_start), mock.patch("lifecycle.time.monotonic", return_value=0), mock.patch(
                    "lifecycle.time.sleep") as sleep:
                self.assertEqual(wait_recovered(supervisor, previous), fresh)
                self.assertEqual(supervisor.identity.call_count, 5)
                self.assertEqual(sleep.call_count, 4)

    def test_missing_recovery_fails_at_the_deadline(self):
        previous = {"pid": 4123, "start": 90421}
        for unavailable in (False, True):
            identity = mock.Mock(side_effect=ProcessLookupError("no process")) if unavailable else mock.Mock(return_value=previous)
            supervisor = SimpleNamespace(identity=identity)
            with self.subTest(unavailable=unavailable), mock.patch(
                    "lifecycle.time.monotonic", side_effect=[10, 10.4, 11]), mock.patch(
                    "lifecycle.time.sleep") as sleep:
                with self.assertRaisesRegex(RuntimeError, "did not automatically recover within 1 seconds"):
                    wait_recovered(supervisor, previous, timeout=1)
                self.assertEqual(identity.call_count, 2)
                sleep.assert_called_once()


class ProductStatusTests(unittest.TestCase):
    settings = {"peer_id": "fixture-peer", "machine_name": "qa", "network_id": "fixture-network",
                "mesh_url": "http://127.0.0.1:12345"}
    platforms = (
        ("systemd user service", "net.agentiscale.agentiscaled.fixture.service",
         "systemd user unit net.agentiscale.agentiscaled.fixture.service, installed, active",
         "systemd user unit net.agentiscale.agentiscaled.fixture.service, not installed"),
        ("launchd GUI LaunchAgent", "net.agentiscale.agentiscaled.fixture",
         "launchd agent net.agentiscale.agentiscaled.fixture, installed, running",
         "launchd agent net.agentiscale.agentiscaled.fixture, not installed"),
    )

    @staticmethod
    def output(service, mesh):
        return ("Node:     fixture-peer (qa)\nNetwork:  fixture-network\n"
                "Mesh:     http://127.0.0.1:12345\n"
                f"Service:  {service}\nMesh connection: {mesh}\n")

    def check(self, output, supervisor, connected):
        stable, data, env = Path("/fixture/bin/agentiscaled"), Path("/fixture/node"), {"HOME": "/fixture/home"}
        with mock.patch("lifecycle.run", return_value=output) as run:
            fields = check_status(stable, data, env, supervisor, self.settings, connected)
            run.assert_called_once_with(stable, "status", "-data-dir", data, env=env)
            return fields

    def test_connected_and_removed_native_services_are_recognized(self):
        for kind, name, running, removed in self.platforms:
            supervisor = SimpleNamespace(kind=kind, name=name)
            for connected, service, mesh in ((True, running, "connected"), (False, removed, "node not running")):
                with self.subTest(platform=kind, connected=connected):
                    fields = self.check(self.output(service, mesh), supervisor, connected)
                    self.assertEqual(fields["Mesh connection"], mesh)

    def test_wrong_node_identity_or_service_is_rejected(self):
        for kind, name, running, removed in self.platforms:
            supervisor = SimpleNamespace(kind=kind, name=name)
            for connected, service, mesh in ((True, running, "connected"), (False, removed, "node not running")):
                output = self.output(service, mesh)
                for before, after in (("fixture-peer", "another-peer"), ("(qa)", "(another-machine)"),
                                      ("fixture-network", "another-network"),
                                      ("http://127.0.0.1:12345", "http://127.0.0.1:54321"),
                                      (name, name + ".unrelated")):
                    with self.subTest(platform=kind, connected=connected, changed=before):
                        with self.assertRaisesRegex(ValueError, "identity or native service state"):
                            self.check(output.replace(before, after), supervisor, connected)

    def test_false_connection_or_service_state_is_rejected(self):
        for kind, name, running, removed in self.platforms:
            supervisor = SimpleNamespace(kind=kind, name=name)
            for connected, service, wrong_service, mesh, wrong_mesh in (
                    (True, running, removed, "connected", "node not running"),
                    (False, removed, running, "node not running", "connected")):
                with self.subTest(platform=kind, connected=connected):
                    with self.assertRaisesRegex(ValueError, "did not report the (connected mesh|stopped node)"):
                        self.check(self.output(service, wrong_mesh), supervisor, connected)
                    with self.assertRaisesRegex(ValueError, "identity or native service state"):
                        self.check(self.output(wrong_service, mesh), supervisor, connected)

    def test_missing_or_duplicate_status_fields_are_rejected(self):
        for kind, name, running, _ in self.platforms:
            supervisor = SimpleNamespace(kind=kind, name=name)
            output = self.output(running, "connected")
            for changed in (output.replace("Network:  fixture-network\n", ""), output + "Node: fixture-peer (qa)\n"):
                with self.subTest(platform=kind, output=changed), self.assertRaises(ValueError):
                    self.check(changed, supervisor, True)


if __name__ == "__main__":
    unittest.main()
