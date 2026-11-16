"""Runs install.sh against a stand-in for GitHub's release downloads, piped
into sh as the README shows, and once against the real latest release.

The stand-in serves a fake agentiscaled for this machine's platform, so the
tests cover platform detection, the latest-release redirect, the checksum
check and where the binary lands, without the network.
"""
import hashlib
import http.server
import io
import os
import platform
import subprocess
import tarfile
import tempfile
import threading
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "install.sh"
VERSION = "v9.8.7"
FAKE_BINARY = b"#!/bin/sh\necho 'agentiscaled 9.8.7 (commit test, fake)'\n"


def platform_name():
    system = {"Linux": "linux", "Darwin": "darwin"}[platform.system()]
    arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    if system == "darwin" and arch == "amd64":
        out = subprocess.run(["sysctl", "-n", "hw.optional.arm64"], capture_output=True, text=True)
        if out.stdout.strip() == "1":
            arch = "arm64"
    return f"{system}_{arch}"


def archive(binary):
    buf = io.BytesIO()
    with tarfile.open(fileobj=buf, mode="w:gz") as tar:
        for name, data in (("LICENSE", b"license\n"), ("agentiscaled", binary)):
            info = tarfile.TarInfo(name)
            info.size = len(data)
            info.mode = 0o644  # install.sh makes it executable
            tar.addfile(info, io.BytesIO(data))
    return buf.getvalue()


class Releases(http.server.BaseHTTPRequestHandler):
    files = {}
    requests = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        Releases.requests.append(self.path)
        if self.path == "/releases/latest":
            self.send_response(302)
            self.send_header("Location", f"/releases/tag/{VERSION}")
            self.end_headers()
            return
        if self.path.startswith("/releases/tag/"):
            data = b"release page"
        elif self.path in Releases.files:
            data = Releases.files[self.path]
        else:
            self.send_response(404)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class InstallTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Releases)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.server.server_port}/releases"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        self.tmp = Path(tempfile.mkdtemp())
        self.publish(VERSION, FAKE_BINARY)
        Releases.requests.clear()
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.tmp / "home"),
                    "TMPDIR": str(self.tmp), "AGENTISCALE_RELEASES_URL": self.base,
                    "NO_PROXY": "127.0.0.1", "no_proxy": "127.0.0.1"}

    def publish(self, version, binary, checksum=None):
        name = f"agentiscaled_{version[1:]}_{platform_name()}.tar.gz"
        data = archive(binary)
        digest = checksum or hashlib.sha256(data).hexdigest()
        other = f"agentiscaled_{version[1:]}_windows_amd64.tar.gz"
        Releases.files = {
            f"/releases/download/{version}/{name}": data,
            f"/releases/download/{version}/checksums.txt":
                f"{'0' * 64}  {other}\n{digest}  {name}\n".encode(),
        }

    def install(self, *args, **env):
        # As the README runs it: piped into sh, options after `sh -s --`.
        return subprocess.run(["sh", "-s", "--", *args], input=SCRIPT.read_text(),
                              env={**self.env, **env}, capture_output=True, text=True, timeout=120)

    def test_latest_into_dir(self):
        result = self.install("-dir", str(self.tmp / "bin"))
        self.assertEqual(result.returncode, 0, result.stderr)
        binary = self.tmp / "bin" / "agentiscaled"
        self.assertTrue(os.access(binary, os.X_OK))
        self.assertEqual(binary.read_bytes(), FAKE_BINARY)
        self.assertIn(f"Downloading agentiscaled {VERSION} for {platform_name().replace('_', '/')}", result.stdout)
        self.assertIn(f"Installed agentiscaled 9.8.7 (commit test, fake) to {binary}", result.stdout)
        self.assertIn("is not on your PATH", result.stdout)
        self.assertIn("/releases/latest", Releases.requests)
        self.assertEqual(sorted(os.listdir(self.tmp / "bin")), ["agentiscaled"])
        self.assertEqual([p for p in os.listdir(self.tmp) if p.startswith("agentiscaled-install")], [])

    def test_version_and_dir_from_environment(self):
        self.publish("v1.2.3", FAKE_BINARY)
        bin_dir = self.tmp / "envbin"
        result = self.install(AGENTISCALE_VERSION="v1.2.3", AGENTISCALE_INSTALL_DIR=str(bin_dir),
                              PATH=f"{bin_dir}:{os.environ['PATH']}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((bin_dir / "agentiscaled").is_file())
        self.assertNotIn("/releases/latest", Releases.requests)
        self.assertNotIn("is not on your PATH", result.stdout)

    def test_option_forms(self):
        self.publish("v1.2.3", FAKE_BINARY)
        result = self.install("--version=v1.2.3", f"-dir={self.tmp / 'b'}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.tmp / "b" / "agentiscaled").is_file())

    def test_default_dir_is_local_bin_without_usr_local_access(self):
        if os.access("/usr/local/bin", os.W_OK):
            self.skipTest("/usr/local/bin is writable here")
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.tmp / "home" / ".local" / "bin" / "agentiscaled").is_file())

    def test_replaces_an_existing_binary(self):
        (self.tmp / "bin").mkdir()
        (self.tmp / "bin" / "agentiscaled").write_text("old")
        result = self.install("-dir", str(self.tmp / "bin"))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.tmp / "bin" / "agentiscaled").read_bytes(), FAKE_BINARY)

    def test_checksum_mismatch_installs_nothing(self):
        self.publish(VERSION, FAKE_BINARY, checksum="f" * 64)
        result = self.install("-dir", str(self.tmp / "bin"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch", result.stderr)
        self.assertFalse((self.tmp / "bin" / "agentiscaled").exists())

    def test_refusals(self):
        for args, message in [
            (["-version", "main"], "version must be a release like v0.11.0"),
            (["-version", "v0.0.1"], "couldn't download"),
            (["-version"], "-version needs a value"),
            (["-force"], "unknown option -force"),
        ]:
            with self.subTest(args):
                result = self.install("-dir", str(self.tmp / "bin"), *args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse((self.tmp / "bin" / "agentiscaled").exists())

    @unittest.skipUnless(os.environ.get("INSTALL_TEST_LIVE") == "1", "set INSTALL_TEST_LIVE=1 to install the real latest release")
    def test_real_latest_release(self):
        env = {k: v for k, v in os.environ.items() if k != "AGENTISCALE_RELEASES_URL"}
        result = subprocess.run(["sh", "-s", "--", "-dir", str(self.tmp / "real")], input=SCRIPT.read_text(),
                                env=env, capture_output=True, text=True, timeout=300)
        self.assertEqual(result.returncode, 0, result.stderr)
        out = subprocess.run([str(self.tmp / "real" / "agentiscaled"), "version"], capture_output=True, text=True)
        self.assertRegex(out.stdout, r"^agentiscaled \d+\.\d+\.\d+")


if __name__ == "__main__":
    unittest.main()
