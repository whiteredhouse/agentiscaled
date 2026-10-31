"""Negative controls for process evidence; exercised by CI, never a service shim."""
import copy
import ctypes
import errno
from pathlib import Path
import unittest
from unittest import mock

from lifecycle_platform import (DarwinSupervisor, ProcBSDInfo, darwin_process, native_target, parse_launchctl, parse_linux_start,
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


if __name__ == "__main__":
    unittest.main()
