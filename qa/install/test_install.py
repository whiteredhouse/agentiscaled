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
import shutil
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


class Base(unittest.TestCase):
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

    def publish(self, version, binary, checksum=None, packages=False):
        v = version[1:]
        assets = {f"agentiscaled_{v}_{platform_name()}.tar.gz": archive(binary)}
        if packages:
            # The stand-in package managers "install" a package by copying
            # it, so a package here is the binary itself.
            assets[f"agentiscaled_{v}_linux_amd64.tar.gz"] = archive(binary)
            assets[f"agentiscaled_{v}_linux_arm64.tar.gz"] = archive(binary)
            for name in (f"agentiscaled_{v}_amd64.deb", f"agentiscaled_{v}_arm64.deb",
                         f"agentiscaled-{v}-1.x86_64.rpm", f"agentiscaled-{v}-1.aarch64.rpm"):
                assets[name] = binary
        lines = [f"{'0' * 64}  agentiscaled_{v}_windows_amd64.tar.gz"]
        for name, data in assets.items():
            lines.append(f"{checksum or hashlib.sha256(data).hexdigest()}  {name}")
        Releases.files = {f"/releases/download/{version}/{n}": d for n, d in assets.items()}
        Releases.files[f"/releases/download/{version}/checksums.txt"] = ("\n".join(lines) + "\n").encode()

    def install(self, *args, **env):
        # As the README runs it: piped into sh, options after `sh -s --`.
        return subprocess.run(["sh", "-s", "--", *args], input=SCRIPT.read_text(),
                              env={**self.env, **env}, capture_output=True, text=True, timeout=120)



class InstallTest(Base):
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
        result = self.install("-method", "binary")
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
            (["-method", "nope"], "method must be auto, package or binary"),
            (["-method", "package"], "-dir installs the binary without a package"),
        ]:
            with self.subTest(args):
                result = self.install("-dir", str(self.tmp / "bin"), *args)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertFalse((self.tmp / "bin" / "agentiscaled").exists())

    def test_cut_short_download_runs_nothing(self):
        text = SCRIPT.read_text()
        last_call = text.rindex('main "$@"')
        for cut in (last_call + len("main"), last_call + len('main "$@"\n'), len(text) // 2):
            with self.subTest(cut=cut):
                # curl | sh with the stream ending early; the pin must not
                # fall back to the latest release.
                result = subprocess.run(["sh", "-s", "--", "-version", "v1.2.3", "-dir", str(self.tmp / "bin")],
                                        input=text[:cut], env=self.env, capture_output=True, text=True, timeout=60)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(Releases.requests, [])
                self.assertFalse((self.tmp / "bin").exists())

    def test_directory_in_the_way_fails(self):
        (self.tmp / "bin" / "agentiscaled").mkdir(parents=True)
        result = self.install("-dir", str(self.tmp / "bin"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("is a directory", result.stderr)
        self.assertEqual(os.listdir(self.tmp / "bin" / "agentiscaled"), [])

    def test_binary_that_does_not_run_fails(self):
        self.publish(VERSION, b"#!/bin/sh\necho 'exec format error' >&2\nexit 126\n")
        result = self.install("-dir", str(self.tmp / "bin"))
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("doesn't run: exec format error", result.stderr)
        self.assertNotIn("Installed", result.stdout)

    def test_broken_sha256sum_falls_back(self):
        stub = self.tmp / "stub"
        stub.mkdir()
        (stub / "sha256sum").write_text("#!/bin/sh\necho 'Bad CPU type in executable' >&2\nexit 126\n")
        (stub / "sha256sum").chmod(0o755)
        if not (shutil.which("shasum") or shutil.which("openssl")):
            self.skipTest("no shasum or openssl to fall back to")
        result = self.install("-dir", str(self.tmp / "bin"), PATH=f"{stub}:{os.environ['PATH']}")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue((self.tmp / "bin" / "agentiscaled").is_file())

    @unittest.skipUnless(os.environ.get("INSTALL_TEST_LIVE") == "1", "set INSTALL_TEST_LIVE=1 to install the real latest release")
    def test_real_latest_release(self):
        env = {k: v for k, v in os.environ.items() if k != "AGENTISCALE_RELEASES_URL"}
        result = subprocess.run(["sh", "-s", "--", "-dir", str(self.tmp / "real")], input=SCRIPT.read_text(),
                                env=env, capture_output=True, text=True, timeout=300)
        self.assertEqual(result.returncode, 0, result.stderr)
        out = subprocess.run([str(self.tmp / "real" / "agentiscaled"), "version"], capture_output=True, text=True)
        self.assertRegex(out.stdout, r"^agentiscaled \d+\.\d+\.\d+")


    @unittest.skipUnless(os.environ.get("INSTALL_TEST_LIVE") == "1" and platform.system() == "Linux"
                         and shutil.which("apt-get"), "set INSTALL_TEST_LIVE=1 on Linux with apt to install the real package")
    def test_real_latest_package(self):
        if os.getuid() != 0 and subprocess.run(["sudo", "-n", "true"], capture_output=True).returncode != 0:
            self.skipTest("needs root or passwordless sudo")
        env = {k: v for k, v in os.environ.items() if k != "AGENTISCALE_RELEASES_URL"}
        result = subprocess.run(["sh", "-s", "--", "-method", "package"], input=SCRIPT.read_text(),
                                env=env, capture_output=True, text=True, timeout=300)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("Installing it with apt-get", result.stdout)
        status = subprocess.run(["dpkg-query", "-W", "-f", "${Status}", "agentiscaled"], capture_output=True, text=True)
        self.assertEqual(status.stdout, "install ok installed")
        out = subprocess.run(["/usr/bin/agentiscaled", "version"], capture_output=True, text=True)
        self.assertRegex(out.stdout, r"^agentiscaled \d+\.\d+\.\d+")


TOOLS = ["sh", "curl", "grep", "mktemp", "rm", "chmod", "awk", "sha256sum", "shasum", "openssl",
         "tar", "cp", "mv", "head", "mkdir", "cat", "sysctl", "env", "gzip"]


class PackageTest(Base):
    """Runs install.sh on a stand-in Linux: uname, id, sudo and the package
    managers are scripts that record how they were called."""

    def machine(self, system="Linux", arch="x86_64", uid=1000, managers=("apt-get",), sudo=True,
                installs=True):
        stub = self.tmp / "stub"
        stub.mkdir()
        for tool in TOOLS:
            found = shutil.which(tool)
            if found:
                (stub / tool).symlink_to(found)
        log = self.tmp / "calls"
        self.pkg_bin = self.tmp / "usr-bin" / "agentiscaled"
        self.pkg_bin.parent.mkdir()

        def script(name, body):
            (stub / name).write_text("#!/bin/sh\n" + body)
            (stub / name).chmod(0o755)

        script("uname", f'[ "$1" = -s ] && echo {system} || echo {arch}\n')
        script("id", f"echo {uid}\n")
        if sudo:
            script("sudo", f'echo "sudo $*" >>{log}\nexec "$@"\n')
        copy = f'for a; do last=$a; done\ncp "$last" {self.pkg_bin}\nchmod 755 {self.pkg_bin}\n'
        for name in managers:
            script(name, f'echo "{name} $*" >>{log}\n' + (copy if installs else ""))
        self.env["PATH"] = str(stub)
        self.env["AGENTISCALE_PACKAGE_BIN"] = str(self.pkg_bin)
        self.publish(VERSION, FAKE_BINARY, packages=True)
        return log

    def calls(self, log):
        return log.read_text().splitlines() if log.exists() else []

    def test_apt_through_sudo(self):
        log = self.machine()
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("Installing it with apt-get through sudo", result.stdout)
        calls = self.calls(log)
        self.assertEqual(len(calls), 2, calls)
        self.assertRegex(calls[0], r"^sudo apt-get install -y --allow-downgrades /.*/agentiscaled_9\.8\.7_amd64\.deb$")
        self.assertEqual(calls[1], calls[0][len("sudo "):])
        self.assertEqual(self.pkg_bin.read_bytes(), FAKE_BINARY)
        self.assertIn(f"Installed agentiscaled 9.8.7 (commit test, fake) to {self.pkg_bin}", result.stdout)

    def test_dnf_as_root_on_arm64(self):
        log = self.machine(arch="aarch64", uid=0, managers=("dnf", "yum"))
        result = self.install()
        self.assertEqual(result.returncode, 0, result.stderr)
        calls = self.calls(log)
        self.assertEqual(len(calls), 1, calls)
        self.assertRegex(calls[0], r"^dnf install -y /.*/agentiscaled-9\.8\.7-1\.aarch64\.rpm$")

    def test_yum(self):
        log = self.machine(managers=("yum",))
        self.assertEqual(self.install().returncode, 0)
        self.assertRegex(self.calls(log)[-1], r"^yum install -y /.*/agentiscaled-9\.8\.7-1\.x86_64\.rpm$")

    def test_zypper(self):
        log = self.machine(managers=("zypper",))
        self.assertEqual(self.install().returncode, 0)
        self.assertRegex(self.calls(log)[-1],
                         r"^zypper --non-interactive install --allow-unsigned-rpm /.*/agentiscaled-9\.8\.7-1\.x86_64\.rpm$")

    def test_dir_or_binary_method_skips_the_package(self):
        log = self.machine()
        for args in (["-dir", str(self.tmp / "bin")], ["-method", "binary", "-dir", str(self.tmp / "bin2")]):
            with self.subTest(args):
                result = self.install(*args)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn("Downloading agentiscaled v9.8.7 for linux/amd64", result.stdout)
        self.assertEqual(self.calls(log), [])

    def test_package_checksum_mismatch_installs_nothing(self):
        log = self.machine()
        self.publish(VERSION, FAKE_BINARY, checksum="f" * 64, packages=True)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("checksum mismatch for agentiscaled_9.8.7_amd64.deb", result.stderr)
        self.assertEqual(self.calls(log), [])

    def test_package_manager_keeping_another_version_fails(self):
        self.machine(installs=False)
        self.pkg_bin.write_text("#!/bin/sh\necho 'agentiscaled 10.0.0 (commit newer, fake)'\n")
        self.pkg_bin.chmod(0o755)
        result = self.install()
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("apt-get kept another agentiscaled version", result.stderr)

    def test_package_refusals(self):
        for machine, message in [
            ({"system": "Darwin"}, "no packages for macOS"),
            ({"managers": ()}, "found no apt-get, dnf, yum or zypper"),
            ({"sudo": False}, "needs root, and there is no sudo or doas"),
        ]:
            with self.subTest(message):
                shutil.rmtree(self.tmp / "stub", ignore_errors=True)
                shutil.rmtree(self.tmp / "usr-bin", ignore_errors=True)
                log = self.machine(**machine)
                result = self.install("-method", "package")
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stderr)
                self.assertEqual(self.calls(log), [])


if __name__ == "__main__":
    unittest.main()
