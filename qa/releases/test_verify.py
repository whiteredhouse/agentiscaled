"""Negative controls for release evidence validation; CI only, no node execution."""
import hashlib
import io
import json
from pathlib import Path
import subprocess
import tarfile
import tempfile
import sys
import unittest
from unittest import mock

from native import run, sentinels, unchanged
from verify import PINS, archive, check_all, download, load_pins, manifest, names, provenance


class ReleaseGuards(unittest.TestCase):
    def test_native_command_timeout_is_finite_and_fails_closed(self):
        with mock.patch("native.subprocess.run", side_effect=subprocess.TimeoutExpired(["fixture"], 300)) as command:
            with self.assertRaisesRegex(RuntimeError, "timed out after 300s"):
                run("fixture")
        self.assertEqual(command.call_args.kwargs["timeout"], 300)

    def test_provenance_timeouts_have_bounded_retries(self):
        with mock.patch("verify.subprocess.run", side_effect=subprocess.TimeoutExpired(["gh"], 180)) as command, mock.patch("verify.time.sleep"):
            with self.assertRaisesRegex(RuntimeError, "provenance timed out"):
                provenance(Path("fixture.deb"), PINS["releases"][0])
        self.assertEqual(command.call_count, 3)
        self.assertTrue(all(call.kwargs["timeout"] == 180 for call in command.call_args_list))

    def response(self, data, length=None):
        response = io.BytesIO(data)
        response.url = "https://example.invalid/asset"
        response.headers = {} if length is None else {"Content-Length": str(length)}
        return response

    def test_download_publishes_only_verified_complete_bytes(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "asset"
            with mock.patch("verify.urllib.request.urlopen", return_value=self.response(b"complete", 8)):
                self.assertEqual(download("https://example.invalid/asset", destination, hashlib.sha256(b"complete").hexdigest()), b"complete")
            self.assertEqual(list(destination.parent.iterdir()), [destination])

    def test_corrupt_truncated_and_oversized_downloads_never_enter_cache(self):
        for data, length, expected, limit in ((b"wrong", 5, hashlib.sha256(b"right").hexdigest(), 100),
                                               (b"short", 10, None, 100),
                                               (b"oversized", None, None, 4),
                                               (b"ignored", 101, None, 100)):
            with self.subTest(data=data, length=length), tempfile.TemporaryDirectory() as directory:
                destination = Path(directory) / "asset"
                with mock.patch("verify.MAX_DOWNLOAD_BYTES", limit), mock.patch("verify.urllib.request.urlopen", return_value=self.response(data, length)):
                    with self.assertRaises(ValueError):
                        download("https://example.invalid/asset", destination, expected)
                self.assertFalse(destination.exists())
                self.assertEqual(list(destination.parent.iterdir()), [])

    def test_interrupted_download_removes_partial_file(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "asset"
            response = mock.MagicMock()
            response.__enter__.return_value = response
            response.url, response.headers = "https://example.invalid/asset", {}
            response.read1.side_effect = [b"partial", OSError("disconnected")] * 3
            with mock.patch("verify.urllib.request.urlopen", return_value=response), mock.patch("verify.time.sleep"):
                with self.assertRaisesRegex(OSError, "disconnected"):
                    download("https://example.invalid/asset", destination)
            self.assertEqual(list(destination.parent.iterdir()), [])

    def test_download_total_deadline_is_bounded(self):
        with tempfile.TemporaryDirectory() as directory:
            destination = Path(directory) / "asset"
            with mock.patch("verify.urllib.request.urlopen", side_effect=lambda *args, **kwargs: self.response(b"late")), mock.patch("verify.time.monotonic", side_effect=[0, 121] * 3), mock.patch("verify.time.sleep"):
                with self.assertRaisesRegex(TimeoutError, "total deadline"):
                    download("https://example.invalid/asset", destination)
            self.assertEqual(list(destination.parent.iterdir()), [])

    def test_runtime_pins_manifest_and_mutable_environment_rejection(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "pins.json"
            path.write_text(json.dumps(PINS))
            with mock.patch.dict("os.environ", {"AGENTISCALE_RELEASE_PINS": str(path)}):
                self.assertEqual(load_pins(), PINS)
            pins = json.loads(json.dumps(PINS))
            pins["environment"]["package_images"]["deb"] = "ubuntu:24.04"
            path.write_text(json.dumps(pins))
            with self.assertRaisesRegex(ValueError, "immutable digest"):
                load_pins(path)

    def test_command_diagnostics_do_not_corrupt_data_stdout(self):
        self.assertEqual(run(sys.executable, "-c", 'import sys; print("payload"); print("diagnostic", file=sys.stderr)'), "payload")

    def release(self, data):
        return {"version": "0.1.11", "manifest_sha256": hashlib.sha256(data).hexdigest()}

    def manifest_bytes(self):
        return "".join("a" * 64 + "  " + name + "\n" for name in sorted(names("0.1.11"))).encode()

    def test_exact_manifest(self):
        data = self.manifest_bytes()
        self.assertEqual(set(manifest(data, self.release(data))), names("0.1.11"))

    def test_mutated_manifest(self):
        data = self.manifest_bytes()
        with self.assertRaisesRegex(ValueError, "reviewed digest"):
            manifest(data + b"\n", self.release(data))

    def test_mutated_cached_asset(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "package"
            path.write_bytes(b"mutated")
            with self.assertRaisesRegex(ValueError, "asset checksum mismatch"):
                download("https://example.invalid/not-requested", path, hashlib.sha256(b"original").hexdigest())

    def test_missing_extra_duplicate_and_traversal_names(self):
        data = self.manifest_bytes()
        cases = [b"\n".join(data.splitlines()[1:]) + b"\n",
                 data + b"b" * 64 + b"  extra.rpm\n",
                 data + data.splitlines(keepends=True)[0],
                 data + b"c" * 64 + b"  ../escape\n"]
        for invalid in cases:
            with self.subTest(invalid=invalid[-100:]), self.assertRaises(ValueError):
                manifest(invalid, self.release(invalid))

    def test_archive_guards(self):
        legal = {"LICENSE": b"fixture-license", "NOTICE": b"fixture-notice", "README.md": b"readme", "agentiscaled": b"binary"}
        cases = [("valid", legal, None, False),
                 ("missing notice", {k: v for k, v in legal.items() if k != "NOTICE"}, None, True),
                 ("bad license", {**legal, "LICENSE": b"wrong"}, None, True),
                 ("bad notice", {**legal, "NOTICE": b"wrong"}, None, True),
                 ("traversal", {**legal, "../escape": b"x"}, None, True),
                 ("symlink", legal, "agentiscaled", True),
                 ("not executable", legal, "mode", True),
                 ("duplicate", legal, "duplicate", True)]
        with tempfile.TemporaryDirectory() as directory:
            for name, files, special, reject in cases:
                with self.subTest(name=name):
                    path = Path(directory) / "archive.tar.gz"
                    with tarfile.open(path, "w:gz") as tar:
                        for filename, content in files.items():
                            info = tarfile.TarInfo(filename)
                            info.size = len(content)
                            if filename == "agentiscaled":
                                info.mode = 0o644 if special == "mode" else 0o755
                            if filename == special:
                                info.type = tarfile.SYMTYPE
                                info.linkname = "/tmp/unexpected"
                                info.size = 0
                            tar.addfile(info, io.BytesIO(content))
                        if special == "duplicate":
                            info = tarfile.TarInfo("NOTICE")
                            tar.addfile(info, io.BytesIO())
                    call = lambda: archive(path, license_digest=hashlib.sha256(legal["LICENSE"]).hexdigest(), notice_digest=hashlib.sha256(legal["NOTICE"]).hexdigest())
                    if reject:
                        with self.assertRaises(ValueError):
                            call()
                    else:
                        self.assertEqual(call(), legal)

    def test_mutated_state_is_not_preserved(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory) / "state"
            snapshot = sentinels([file])
            unchanged(snapshot)
            file.write_text("changed")
            with self.assertRaisesRegex(ValueError, "modified sentinel"):
                unchanged(snapshot)
            file.unlink()
            with self.assertRaises(ValueError):
                unchanged(snapshot)

    def test_existing_state_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            file = Path(directory) / "state"
            file.write_text("keep")
            with self.assertRaisesRegex(ValueError, "overwrite"):
                sentinels([file])
            self.assertEqual(file.read_text(), "keep")

    def test_partial_evidence_survives_later_failure(self):
        rows, status = [], {}
        with tempfile.TemporaryDirectory() as directory:
            manifest_result = ("https://example.invalid", Path(directory) / "checksums.txt", {"a.deb": "a" * 64, "b.rpm": "b" * 64})
            with mock.patch("verify.fetch_manifest", return_value=manifest_result), mock.patch("verify.download"), mock.patch("verify.provenance", side_effect=[None, None, RuntimeError("fixture provenance refusal")]):
                with self.assertRaisesRegex(RuntimeError, "fixture provenance refusal"):
                    check_all(directory, rows, status)
        self.assertEqual([r["asset"] for r in rows], ["a.deb"])
        self.assertEqual((status["asset"], status["stage"]), ("b.rpm", "provenance"))


if __name__ == "__main__":
    unittest.main()
