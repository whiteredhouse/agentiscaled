"""Runs the action's join.sh against a stand-in for GitHub's OIDC token
endpoint and for the control service, with a real agentiscaled release.

The control stand-in refuses the enrollment as a network that does not trust
the workflow would, so the test covers everything up to the node's start:
the release is fetched and checked, the token is asked for this network's
audience, masked, sent once and removed, and the refusal fails the step.
"""
import http.server
import json
import os
import platform
import subprocess
import tempfile
import threading
import unittest
import urllib.parse
from pathlib import Path

HERE = Path(__file__).resolve().parent
NETWORK = "6352a29c-a8fe-4288-bca8-f483059900d1"
TOKEN = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ0ZXN0In0.c2lnbmF0dXJl"
REQUEST_TOKEN = "request-token"


class Fake(http.server.BaseHTTPRequestHandler):
    calls = []

    def log_message(self, *args):
        pass

    def reply(self, status, body):
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        url = urllib.parse.urlsplit(self.path)
        Fake.calls.append(("GET", url.path, dict(urllib.parse.parse_qsl(url.query)),
                           self.headers.get("Authorization")))
        if url.path == "/token":
            self.reply(200, {"count": 1, "value": TOKEN})
        else:
            self.reply(404, {"error": "not found"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        Fake.calls.append(("POST", self.path, body, None))
        self.reply(403, {"error": "no trust in this network matches the workflow"})


def runner():
    os_name = {"Linux": "Linux", "Darwin": "macOS"}[platform.system()]
    arch = {"x86_64": "X64", "AMD64": "X64", "arm64": "ARM64", "aarch64": "ARM64"}[platform.machine()]
    return os_name, arch


class JoinTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Fake)
        threading.Thread(target=cls.server.serve_forever, daemon=True).start()
        cls.base = f"http://127.0.0.1:{cls.server.server_port}"

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()

    def setUp(self):
        Fake.calls.clear()
        self.tmp = Path(tempfile.mkdtemp())
        os_name, arch = runner()
        self.env = {
            "PATH": os.environ["PATH"], "HOME": str(self.tmp),
            "RUNNER_OS": os_name, "RUNNER_ARCH": arch, "RUNNER_TEMP": str(self.tmp),
            "GITHUB_PATH": str(self.tmp / "path"), "GITHUB_OUTPUT": str(self.tmp / "output"),
            "ACTIONS_ID_TOKEN_REQUEST_URL": self.base + "/token?api-version=2.0",
            "ACTIONS_ID_TOKEN_REQUEST_TOKEN": REQUEST_TOKEN,
            "AGENTISCALE_NETWORK": NETWORK, "AGENTISCALE_DATA_DIR": str(self.tmp / "node"),
            "AGENTISCALE_CONTROL": self.base + "//", "AGENTISCALE_HOSTNAME": "github-test",
            "AGENTISCALE_TIMEOUT": "10",
        }
        for name in ("HTTPS_PROXY", "https_proxy", "SSL_CERT_FILE", "CURL_CA_BUNDLE"):
            if name in os.environ:
                self.env[name] = os.environ[name]
        # Loopback stand-ins only; the release still comes from GitHub.
        self.env["NO_PROXY"] = self.env["no_proxy"] = "127.0.0.1"

    def join(self, **env):
        return subprocess.run([str(HERE / "join.sh")], env={**self.env, **env},
                              capture_output=True, text=True, timeout=300)

    def test_refused_enrollment_fails_after_one_masked_token(self):
        result = self.join(AGENTISCALE_ACTION_REF="main")
        out = result.stdout + result.stderr
        self.assertNotEqual(result.returncode, 0, out)
        self.assertIn(f"::add-mask::{TOKEN}", result.stdout)
        self.assertIn("no trust in this network matches the workflow", out)

        gets = [c for c in Fake.calls if c[0] == "GET"]
        self.assertEqual(len(gets), 1, Fake.calls)
        _, path, query, auth = gets[0]
        self.assertEqual(path, "/token")
        self.assertEqual(query, {"api-version": "2.0", "audience": f"{self.base}/networks/{NETWORK}"})
        self.assertEqual(auth, f"Bearer {REQUEST_TOKEN}")
        posts = [c for c in Fake.calls if c[0] == "POST"]
        self.assertTrue(posts, Fake.calls)
        for _, path, body, _ in posts:
            self.assertEqual(path, "/v1/enroll/oidc")
            self.assertEqual(body["id_token"], TOKEN)
            self.assertEqual(body["network_id"], NETWORK)
            self.assertEqual(body["machine_name"], "github-test")

        # The token is gone; the verified binary is on the job's PATH.
        self.assertFalse((self.tmp / "node" / "oidc-token").exists())
        install = Path((self.tmp / "path").read_text().strip())
        self.assertTrue((install / "agentiscaled").is_file())
        self.assertRegex(install.name, r"^agentiscaled-v\d+\.\d+\.\d+$")

    def test_pinned_tag_installs_that_release(self):
        result = self.join(AGENTISCALE_ACTION_REF="v0.11.0")
        self.assertIn("agentiscaled-v0.11.0", (self.tmp / "path").read_text(), result.stdout + result.stderr)

    def test_checks_before_asking_for_a_token(self):
        for env, message in [
            ({"AGENTISCALE_NETWORK": "my-network"}, "network-id must be your network's ID"),
            ({"ACTIONS_ID_TOKEN_REQUEST_URL": ""}, "id-token: write"),
            ({"RUNNER_OS": "Windows"}, "Linux and macOS runners"),
            ({"AGENTISCALE_VERSION": "v0.10.0"}, "v0.11.0 or later"),
            ({"AGENTISCALE_VERSION": "main"}, "a release like v0.12.0"),
        ]:
            with self.subTest(message):
                Fake.calls.clear()
                result = self.join(**env)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn(message, result.stdout)
                self.assertEqual(Fake.calls, [])


if __name__ == "__main__":
    unittest.main()
