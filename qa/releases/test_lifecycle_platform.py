"""Negative controls for process evidence; exercised by CI, never a service shim."""
import copy
import ctypes
import errno
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from lifecycle_platform import (DarwinSupervisor, LinuxSupervisor, ProcBSDInfo, darwin_process, native_target, parse_launchctl, parse_linux_start,
                                parse_lsof, validate_fresh_process, validate_mapping)


class ProcessEvidenceTests(unittest.TestCase):
    def test_native_platform_matrix_rejects_unsupported_or_translated_hosts(self):
        for system, machine, expected in (("Linux", "x86_64", ("linux", "amd64")),
                                           ("Linux", "aarch64", ("linux", "arm64")),
                                           ("Darwin", "x86_64", ("darwin", "amd64")),
                                           ("Darwin", "arm64", ("darwin", "arm64"))):
            self.assertEqual(native_target(system, machine), expected)
        for system, machine in (("Windows", "AMD64"), ("Linux", "i686"), ("Darwin", "aarch64")):
            with self.assertRaises(ValueError):
                native_target(system, machine)
        with self.assertRaisesRegex(ValueError, "translated"):
            native_target("Darwin", "x86_64", apple_arm64=True)

    def test_launchctl_reads_top_level_pid_not_nested_state(self):
        output = """gui/501/net.agentiscale.agentiscaled.123 = {
    state = running
    pid = 4123
    event triggers = {
        state = waiting
        pid = 999
    }
}
"""
        self.assertEqual(parse_launchctl(output)["pid"], 4123)
        for invalid in (output.replace("state = running", "state = waiting"),
                        output.replace("pid = 4123", "pid = 0"),
                        output.replace("pid = 4123", "pid = 4123\n    pid = 4124"),
                        "state = running\npid = 4123"):
            with self.assertRaises(ValueError):
                parse_launchctl(invalid)

    def test_linux_start_handles_spaces_and_parentheses_in_comm(self):
        fields = ["S"] + ["0"] * 18 + ["90421", "0"]
        self.assertEqual(parse_linux_start("4123 (node (old) name) " + " ".join(fields)), 90421)
        with self.assertRaises(ValueError):
            parse_linux_start("4123 (node) S 0")

    def test_mapped_image_requires_matching_pid_path_device_and_inode(self):
        expected = {"path": "/private/tmp/node/agentiscaled", "device": 16777234, "inode": 701}
        output = "p4123\nftxt\nD0x1000012\ni701\nn/private/tmp/node/agentiscaled\n"
        rows = parse_lsof(output, 4123)
        self.assertEqual(validate_mapping(rows, expected)["inode"], 701)
        for field, value in (("inode", 700), ("device", 1), ("path", "/tmp/other")):
            changed = dict(expected, **{field: value})
            with self.assertRaises(ValueError):
                validate_mapping(rows, changed)
        with self.assertRaises(ValueError):
            parse_lsof(output.replace("p4123", "p4124"), 4123)
        with self.assertRaises(ValueError):
            validate_mapping(parse_lsof(output.replace("ftxt", "f3"), 4123), expected)
        with self.assertRaises(ValueError):
            validate_mapping(parse_lsof(output.replace("i701\n", ""), 4123), expected)
        with self.assertRaises(ValueError):
            validate_mapping(parse_lsof(output.replace("agentiscaled\n", "agentiscaled (deleted)\n"), 4123), expected)

    def test_cleanup_accepts_process_disappearing_between_kill_and_libproc(self):
        supervisor = DarwinSupervisor({}, Path("/tmp/home"), Path("/tmp/node"), Path("/tmp/bin/node"), None)
        identity = {"pid": 4123, "start": [1700000000, 200]}
        with mock.patch("lifecycle_platform.os.kill", return_value=None), mock.patch(
                "lifecycle_platform.darwin_process", side_effect=ProcessLookupError(errno.ESRCH, "exited")):
            self.assertIsNone(supervisor.wait_stopped(identity))
        # An inaccessible or malformed process is not evidence of its exit.
        for error in (PermissionError(errno.EPERM, "denied"), RuntimeError("ABI mismatch")):
            with mock.patch("lifecycle_platform.os.kill", return_value=None), mock.patch(
                    "lifecycle_platform.darwin_process", side_effect=error):
                with self.assertRaises(type(error)):
                    supervisor.wait_stopped(identity)

    def test_libproc_only_classifies_esrch_as_a_disappeared_process(self):
        for operation in ("info", "path"):
            for error_number, expected in ((errno.ESRCH, ProcessLookupError),
                                           (errno.EPERM, RuntimeError), (0, RuntimeError)):
                libproc = mock.Mock()

                def info(pid, flavor, argument, pointer, size):
                    if operation == "info":
                        ctypes.set_errno(error_number)
                        return 0
                    value = ctypes.cast(pointer, ctypes.POINTER(ProcBSDInfo)).contents
                    value.pid, value.uid, value.start_sec = pid, 501, 1700000000
                    return size

                def path(*args):
                    ctypes.set_errno(error_number)
                    return 0

                libproc.proc_pidinfo.side_effect = info
                libproc.proc_pidpath.side_effect = path
                with self.subTest(operation=operation, errno=error_number), mock.patch(
                        "lifecycle_platform.ctypes.CDLL", return_value=libproc), mock.patch(
                        "lifecycle_platform.os.getuid", return_value=501):
                    with self.assertRaises(expected):
                        darwin_process(4123)

    def test_fresh_start_guard_rejects_reused_or_unstable_process(self):
        before = {"pid": 4123, "start": [1700000000, 200]}
        after = copy.deepcopy(before)
        validate_fresh_process(before, after, previous={"pid": 4122, "start": [1700000000, 100]}, since_us=1700000000000100)
        for final, previous, since in ((dict(after, pid=4124), None, 0),
                                      (dict(after, start=[1700000000, 201]), None, 0),
                                      (after, before, 0),
                                      (after, None, 1700000000000300)):
            with self.assertRaises(ValueError):
                validate_fresh_process(before, final, previous=previous, since_us=since)


class NativeServiceControlTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name)

    def supervisor(self, supervisor_type, root, run):
        return supervisor_type({"XDG_RUNTIME_DIR": str(root / "runtime")}, root / "home",
                               root / "node", root / "bin/node", run)

    def test_crash_signals_only_the_unchanged_native_service(self):
        previous = {"pid": 4123, "start": [1700000000, 200]}
        for supervisor_type, command, method in (
                (LinuxSupervisor, ("systemctl", "--user", "kill", "--kill-whom=main", "--signal=SIGKILL"),
                 "systemctl --user kill --kill-whom=main --signal=SIGKILL"),
                (DarwinSupervisor, ("/bin/launchctl", "kill", "SIGKILL"), "launchctl kill SIGKILL")):
            run = mock.Mock(return_value="")
            supervisor = self.supervisor(supervisor_type, self.root, run)
            with self.subTest(supervisor=supervisor.kind), mock.patch.object(
                    supervisor, "unchanged_process", return_value=previous) as unchanged:
                self.assertEqual(supervisor.crash(previous), {"signal": "SIGKILL", "method": method})
                unchanged.assert_called_once_with(previous)
                target = supervisor.name if supervisor_type is LinuxSupervisor else supervisor.target
                run.assert_called_once_with(*command, target, env=supervisor.env)

    def test_crash_refuses_a_changed_pid_or_start_before_signaling(self):
        previous = {"pid": 4123, "start": [1700000000, 200]}
        for current in (dict(previous, pid=4124), dict(previous, start=[1700000000, 201])):
            run = mock.Mock(return_value="service = {\n    state = running\n    pid = " + str(current["pid"]) + "\n}\n")
            for supervisor_type in (LinuxSupervisor, DarwinSupervisor):
                supervisor = self.supervisor(supervisor_type, self.root, run)
                with self.subTest(supervisor=supervisor.kind, current=current), mock.patch.object(
                        supervisor, "identity", return_value=current), mock.patch(
                        "lifecycle_platform.darwin_process", return_value=current):
                    run.reset_mock()
                    with self.assertRaisesRegex(ValueError, "PID/start changed"):
                        supervisor.crash(previous)
                    self.assertFalse(any("kill" in call.args for call in run.call_args_list))

    def test_linux_prepare_can_recreate_discovery_after_removal(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "runtime").mkdir()
            run = mock.Mock(return_value="")
            supervisor = self.supervisor(LinuxSupervisor, root, run)
            supervisor.prepare()
            self.assertIn("ExecStart=/usr/bin/false", supervisor.path.read_text())
            supervisor.path.unlink()  # The product, rather than prepare, removes the generated unit.
            supervisor.prepare()
            self.assertEqual(run.call_args_list.count(mock.call(
                "systemctl", "--user", "link", "--runtime", supervisor.path, env=supervisor.env)), 2)

    def test_linux_prepare_refuses_existing_unit_or_runtime_link(self):
        for existing in ("unit", "unit-symlink", "runtime-link"):
            with self.subTest(existing=existing), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                (root / "runtime").mkdir()
                run = mock.Mock()
                supervisor = self.supervisor(LinuxSupervisor, root, run)
                path = supervisor.runtime_link() if existing == "runtime-link" else supervisor.path
                path.parent.mkdir(parents=True)
                if existing == "unit":
                    path.write_text("unrelated existing unit")
                else:
                    path.symlink_to(root / "missing-foreign-target")
                with self.assertRaisesRegex(ValueError, "already exists"):
                    supervisor.prepare()
                self.assertTrue(path.is_symlink() if existing != "unit" else path.read_text() == "unrelated existing unit")
                run.assert_not_called()

    def test_linux_removal_requires_stopped_process_and_native_unloaded_state(self):
        removed = "MainPID=0\nActiveState=inactive\nLoadState=not-found\nFragmentPath=\n"
        previous = {"pid": 4123, "start": 90421}
        for response in (removed, removed.replace("MainPID=0", "MainPID=4123"),
                         removed.replace("ActiveState=inactive", "ActiveState=active"),
                         removed.replace("LoadState=not-found", "LoadState=loaded"),
                         removed.replace("FragmentPath=", "FragmentPath=/tmp/other.service"), ""):
            run = mock.Mock(return_value=response)
            supervisor = self.supervisor(LinuxSupervisor, self.root, run)
            with self.subTest(response=response), mock.patch.object(supervisor, "wait_stopped") as stopped:
                if response == removed:
                    supervisor.assert_removed(previous)
                else:
                    with self.assertRaisesRegex(ValueError, "remains loaded or active"):
                        supervisor.assert_removed(previous)
                stopped.assert_called_once_with(previous)

    def test_removal_rejects_leftover_generated_paths(self):
        for supervisor_type in (LinuxSupervisor, DarwinSupervisor):
            with self.subTest(supervisor=supervisor_type.kind), tempfile.TemporaryDirectory() as directory:
                run = mock.Mock(return_value="")
                supervisor = self.supervisor(supervisor_type, Path(directory), run)
                supervisor.path.parent.mkdir(parents=True)
                supervisor.path.symlink_to(Path(directory) / "missing")
                with mock.patch.object(supervisor, "wait_stopped"), self.assertRaisesRegex(
                        ValueError, "left its generated"):
                    supervisor.assert_removed({"pid": 4123, "start": 90421})
                run.assert_not_called()

    def test_linux_removal_cleans_only_its_own_dangling_discovery_link(self):
        for own_link in (True, False):
            with self.subTest(own_link=own_link), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                run = mock.Mock(return_value="MainPID=0\nActiveState=inactive\nLoadState=not-found\nFragmentPath=\n")
                supervisor = self.supervisor(LinuxSupervisor, root, run)
                link = supervisor.runtime_link()
                link.parent.mkdir(parents=True)
                link.symlink_to(supervisor.path if own_link else root / "foreign-unit")
                with mock.patch.object(supervisor, "wait_stopped"):
                    if own_link:
                        supervisor.assert_removed({"pid": 4123, "start": 90421})
                        self.assertFalse(link.is_symlink())
                        self.assertEqual(run.call_args_list[0], mock.call(
                            "systemctl", "--user", "daemon-reload", env=supervisor.env))
                    else:
                        with self.assertRaisesRegex(ValueError, "no longer belongs"):
                            supervisor.assert_removed({"pid": 4123, "start": 90421})
                        self.assertTrue(link.is_symlink())
                        run.assert_not_called()

    def test_linux_wait_stopped_distinguishes_exit_reuse_and_live_process(self):
        supervisor = self.supervisor(LinuxSupervisor, self.root, None)
        previous = {"pid": 4123, "start": 90421}
        stat = "4123 (node) " + " ".join(["S"] + ["0"] * 18 + ["90421", "0"])
        with mock.patch("lifecycle_platform.Path.read_text", side_effect=FileNotFoundError):
            supervisor.wait_stopped(previous)
        with mock.patch("lifecycle_platform.Path.read_text", return_value=stat.replace("90421", "90422")):
            supervisor.wait_stopped(previous)
        with mock.patch("lifecycle_platform.Path.read_text", return_value=stat), mock.patch(
                "lifecycle_platform.time.monotonic", side_effect=[0, 31]):
            with self.assertRaisesRegex(RuntimeError, "remains alive"):
                supervisor.wait_stopped(previous)
        for error in (PermissionError(errno.EPERM, "denied"), ValueError("malformed stat")):
            with self.subTest(error=error), mock.patch("lifecycle_platform.Path.read_text", side_effect=error):
                with self.assertRaises(type(error)):
                    supervisor.wait_stopped(previous)

    def test_darwin_removal_requires_unloaded_native_service(self):
        for loaded in ("", "service = {\n    state = waiting\n}\n"):
            run = mock.Mock(return_value=loaded)
            supervisor = self.supervisor(DarwinSupervisor, self.root, run)
            previous = {"pid": 4123, "start": [1700000000, 200]}
            with self.subTest(loaded=loaded), mock.patch.object(supervisor, "wait_stopped") as stopped:
                if loaded:
                    with self.assertRaisesRegex(ValueError, "remains loaded"):
                        supervisor.assert_removed(previous)
                else:
                    supervisor.assert_removed(previous)
                stopped.assert_called_once_with(previous)
                run.assert_called_once_with("/bin/launchctl", "print", supervisor.target,
                                            env=supervisor.env, check=False)

    def test_darwin_cleanup_boots_out_loaded_service_even_when_plist_is_missing(self):
        loaded = "service = {\n    state = running\n    pid = 4124\n}\n"
        run = mock.Mock(side_effect=[loaded, "", ""])
        supervisor = self.supervisor(DarwinSupervisor, self.root, run)
        supervisor.prepared = True
        previous = {"pid": 4123, "start": [1700000000, 200]}
        current = {"pid": 4124, "start": [1700000001, 200]}
        supervisor.last_process = previous
        self.assertFalse(supervisor.path.exists())
        with mock.patch("lifecycle_platform.darwin_process", return_value=current) as process, mock.patch.object(
                supervisor, "wait_stopped") as stopped:
            self.assertEqual(supervisor.cleanup(), [])
            process.assert_called_once_with(4124, with_path=False)
            self.assertEqual(stopped.call_args_list, [mock.call(previous), mock.call(current)])
        self.assertEqual(run.call_args_list, [
            mock.call("/bin/launchctl", "print", supervisor.target, env=supervisor.env, check=False),
            mock.call("/bin/launchctl", "bootout", supervisor.target, env=supervisor.env),
            mock.call("/bin/launchctl", "print", supervisor.target, env=supervisor.env, check=False)])

    def test_darwin_cleanup_boots_out_even_when_loaded_process_evidence_fails(self):
        for loaded in ("service = {\n    state = running\n    pid = 4124\n}\n",
                       "service = {\n    state = waiting\n}\n"):
            run = mock.Mock(side_effect=[loaded, "", ""])
            supervisor = self.supervisor(DarwinSupervisor, self.root, run)
            supervisor.prepared = True
            previous = {"pid": 4123, "start": [1700000000, 200]}
            supervisor.last_process = previous
            with self.subTest(loaded=loaded), mock.patch("lifecycle_platform.darwin_process", side_effect=
                    RuntimeError("native identity unavailable")), mock.patch.object(supervisor, "wait_stopped") as stopped:
                errors = supervisor.cleanup()
                self.assertEqual(len(errors), 1)
                self.assertIn("cannot capture launchd cleanup process", errors[0])
                stopped.assert_called_once_with(previous)
                self.assertIn(mock.call("/bin/launchctl", "bootout", supervisor.target, env=supervisor.env),
                              run.call_args_list)


if __name__ == "__main__":
    unittest.main()
