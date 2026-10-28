"""Real enrolled native node upgrade on disposable CI; never rebuild a release.

Only the companion mesh is compiled, from the public source commit pinned for
our newer release. Homebrew-installed published executables must match the
verified native_payloads release bytes exactly.
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import platform
import shutil
import socket
import subprocess
import tarfile
import tempfile
import time
import urllib.request

from verify import PINS, download, native_payloads
from lifecycle_platform import (DarwinSupervisor, LinuxSupervisor, native_target, verified_image)
from lifecycle_install import homebrew_installation


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def run(*command, env=None, cwd=None, timeout=90, check=True):
    print("+", " ".join(map(str, command)), flush=True)
    result = subprocess.run(list(map(str, command)), env=env, cwd=cwd,
                            capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    if result.stdout:
        print(result.stdout, flush=True)
    if result.stderr:
        print(result.stderr, flush=True)
    if check and result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {command[0]}")
    return result.stdout.strip()


def http_json(url):
    # Loopback fixture only: no proxy, credentials, or external control service.
    with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(url, timeout=8) as response:
        return json.load(response)


class UnixHTTP(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("localhost", timeout=3)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(str(self.path))


def node_request(data, path):
    connection = UnixHTTP(data / "agentmesh.sock")
    try:
        connection.request("GET", path)
        response = connection.getresponse()
        body = response.read(1024 * 1024)
        if response.status != 200:
            raise RuntimeError(f"node {path}: HTTP {response.status}")
        return json.loads(body) if path.startswith("/debug/") else None
    finally:
        connection.close()


def wait_ready(data):
    deadline = time.monotonic() + 90
    while True:
        try:
            node_request(data, "/readyz")
            return node_request(data, "/debug/mesh-info")
        except (OSError, RuntimeError, http.client.HTTPException) as exc:
            if time.monotonic() >= deadline:
                raise RuntimeError("node did not become connected/ready within 90 seconds") from exc
            time.sleep(1)


def compile_companion(root, report):
    # A full commit ID is the pin; no branches, tags, or local development tree.
    commit = PINS["releases"][-1]["commit"]
    if len(commit) != 40 or any(c not in "0123456789abcdef" for c in commit):
        raise ValueError("fixture source requires a full immutable Git commit")
    archive = root / "fixture-source.tar.gz"
    download(f'https://codeload.github.com/{PINS["repository"]}/tar.gz/{commit}', archive)
    source = root / "fixture-source"
    source.mkdir()
    total = 0
    with tarfile.open(archive, "r:gz") as tar:
        for member in tar:
            parts = Path(member.name).parts
            if not parts or parts[0] != "agentiscaled-" + commit or ".." in parts or member.name.startswith("/"):
                raise ValueError("unsafe fixture source archive path")
            if not (member.isdir() or member.isfile()):
                raise ValueError("fixture source archive contains a link or special file")
            total += member.size
            if total > 200 * 1024 * 1024:
                raise ValueError("fixture source archive is too large")
            target = source.joinpath(*parts[1:])
            if member.isdir():
                target.mkdir(parents=True, exist_ok=True)
            else:
                target.parent.mkdir(parents=True, exist_ok=True)
                target.write_bytes(tar.extractfile(member).read())
    fixtures = Path(__file__).with_name("testdata")
    # Use the archived mesh module's complete dependency lock, which includes
    # standalone's SQL/console dependencies absent from the client go.mod.
    mesh_source = source / "third_party/agentmesh"
    command = mesh_source / "cmd" / "qa-lifecycle"
    command.mkdir()
    shutil.copyfile(fixtures / "lifecycle_mesh.go", command / "main.go")
    alias = mesh_source / "export/standalone"
    alias.mkdir()
    shutil.copyfile(fixtures / "standalone_alias.go", alias / "standalone.go")
    output = root / "mesh-companion"
    # Build exactly one companion command, never ./cmd/agentiscaled or ./....
    run("go", "build", "-mod=readonly", "-o", output, "./cmd/qa-lifecycle", cwd=mesh_source, timeout=600)
    report["companion"] = {"source_commit": commit, "source_archive_sha256": digest(archive),
                           "command": "go build -mod=readonly -o mesh-companion ./cmd/qa-lifecycle",
                           "release_rebuilt": False}
    return output


def running(supervisor, image, data, release, report, since_us, previous=None):
    # Readiness first lets asynchronous native supervisors finish their start;
    # evidence then sandwiches a second real socket/mesh observation in a
    # stable PID/start-time and mapped-image check.
    wait_ready(data)
    supervisor.validate_generated()
    observation = supervisor.evidence(image, since_us, previous, lambda: wait_ready(data))
    observation.update(main_pid=observation["pid"], version=release["version"], source_commit=release["commit"],
                       expected_release_exe_sha256=image["sha256"], readyz=200)
    report["observations"].append(observation)
    return observation["mesh"]["peer_id"]


def exercise(payloads, root, report, installation):
    env = os.environ.copy()
    home = root / "home"
    home.mkdir()
    # Keep the real user manager's XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS.
    env.update(HOME=str(home), XDG_CONFIG_HOME=str(home / ".config"),
               XDG_DATA_HOME=str(home / ".local/share"), XDG_CACHE_HOME=str(home / ".cache"),
               AGENTISCALE_NO_BROWSER="1")
    data = root / "node"
    data.mkdir(mode=0o700)
    stable = installation.stable
    supervisor_type = LinuxSupervisor if report["system"] == "linux" else DarwinSupervisor
    supervisor = supervisor_type(env, home, data, stable, run)
    process = None
    logs = root / "mesh.log"
    descriptor = root / "mesh-ready.json"
    try:
        report["stage"] = "real native supervisor availability"
        supervisor.prepare()
        companion = compile_companion(root, report)
        report["stage"] = "standalone mesh fixture"
        with logs.open("w") as output:
            process = subprocess.Popen([str(companion), "--data", str(root / "mesh"), "--node-data", str(data),
                                        "--ready", str(descriptor)], stdout=output, stderr=subprocess.STDOUT, env=env)
        deadline = time.monotonic() + 60
        while not descriptor.exists():
            if process.poll() is not None or time.monotonic() >= deadline:
                raise RuntimeError("standalone mesh fixture failed to start within 60 seconds")
            time.sleep(0.2)
        fixture = json.loads(descriptor.read_text())
        old, new = payloads
        if digest(stable) != old[2]:
            raise ValueError("old stable binary differs from verified release")
        for release, folder, _ in payloads:
            expected = f'agentiscaled {release["version"]} (commit {release["commit"][:12]}, {report["system"]}/{report["arch"]})'
            if run(folder / "agentiscaled", "version", env=env) != expected:
                raise ValueError("release executable version differs from reviewed pin")
        report["stage"] = "old release enrollment and actual service"
        old_image = verified_image(stable, old[2])
        since_us = time.time_ns() // 1000
        run(stable, "up", "-data-dir", data, "-auth-issuer", fixture["control"] + "/",
            "-auth-client-id", "ci-client", "-auth-audience", "ci-control", "-control", fixture["control"],
            "-clients", "none", "-listen", "/ip4/127.0.0.1/tcp/0", "-bind-addr", "", "-hostname", "qa", "-y", env=env, timeout=150)
        settings = data / "agentiscaled.json"
        preserved = {path: path.read_bytes() for path in (settings, supervisor.path)}
        report["preserved_files"] = {path.name: hashlib.sha256(value).hexdigest() for path, value in preserved.items()}
        old_peer = running(supervisor, old_image, data, old[0], report, since_us)
        supervisor.stop()
        identity_before = http_json(fixture["control"] + "/inspect")
        if identity_before["peer_id"] != old_peer or identity_before["mesh_url"] != fixture["mesh_url"]:
            raise ValueError("persisted enrolled identity differs from the running node")
        report["identity_before"] = identity_before
        report["stage"] = "old release restart with enrolled identity"
        since_us = time.time_ns() // 1000
        supervisor.start()
        if running(supervisor, old_image, data, old[0], report, since_us, report["observations"][-1]) != old_peer:
            raise ValueError("old release restart changed peer identity")
        # Upgrade the real cask while the old enrolled service still runs.
        # Homebrew removes its old Caskroom executable; the generated service
        # must retain brew/bin and pick up the new release on its next start.
        report["stage"] = "actual Homebrew upgrade while old enrolled service runs"
        new_image = installation.upgrade()
        still_running = supervisor.unchanged_process(report["observations"][-1])
        upgrade_mesh = wait_ready(data)
        supervisor.unchanged_process(still_running)
        if upgrade_mesh["peer_id"] != old_peer:
            raise ValueError("Homebrew upgrade changed the still-running old node identity")
        report["upgrade_method"]["old_service_remained_running"] = {
            "main_pid": still_running["pid"], "start": still_running["start"], "readyz": 200, "mesh": upgrade_mesh}
        since_us = time.time_ns() // 1000
        supervisor.restart()
        if running(supervisor, new_image, data, new[0], report, since_us, report["observations"][-1]) != old_peer:
            raise ValueError("new release changed the enrolled peer identity")
        supervisor.stop()
        identity_after = http_json(fixture["control"] + "/inspect")
        report["identity_after"] = identity_after
        if identity_before != identity_after:
            raise ValueError("upgrade changed persisted enrolled key or identity credential")
        for path, contents in preserved.items():
            if path.read_bytes() != contents:
                raise ValueError(f"upgrade changed persistent settings/service: {path.name}")
        evidence = http_json(fixture["control"] + "/evidence")
        report["mesh_evidence"] = evidence
        if evidence["enrollments"] != [old_peer]:
            raise ValueError("restart/upgrade unexpectedly enrolled a fresh node")
        if any(evidence["router_peer_id"] not in item["mesh"]["attached_routers"] for item in report["observations"]):
            raise ValueError("node was not attached to the real fixture router on every start")
        report["service_unit"] = supervisor.name
        report["service"] = {"kind": supervisor.kind, "name": supervisor.name, "generated_file": str(supervisor.path)}
        report["preservation"] = "real enrolled key, credential, peer ID, settings and service unit preserved"
    finally:
        # Always stop the actual service before deleting its private database.
        # Cleanup errors fail the run: a claimed success cannot leave a node up.
        cleanup_errors = supervisor.cleanup()
        if process is not None:
            process.terminate()
            try:
                process.wait(timeout=15)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        node_log = data / "agentiscaled.log"
        if node_log.exists():
            report["node_log_tail"] = node_log.read_text(errors="replace")[-16000:]
        if logs.exists():
            report["fixture_log_tail"] = logs.read_text(errors="replace")[-16000:]
        report["cleanup"] = "passed" if not cleanup_errors else cleanup_errors
        if cleanup_errors:
            raise RuntimeError("real native supervisor cleanup failed: " + "; ".join(cleanup_errors))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("CI") != "true":
        raise SystemExit("real lifecycle verification runs only on disposable CI")
    apple_arm64 = platform.system() == "Darwin" and run("/usr/sbin/sysctl", "-in", "hw.optional.arm64") == "1"
    system, arch = native_target(platform.system(), platform.machine(), apple_arm64=apple_arm64)
    args.directory.mkdir(parents=True, exist_ok=True)
    supervisor_kind = "systemd user service" if system == "linux" else "launchd GUI LaunchAgent"
    report = {"success": False, "system": system, "arch": arch, "runner": platform.platform(),
              "observations": [], "scope": f"native {system}/{arch} real {supervisor_kind}; enrolled release upgrade"}
    try:
        report["stage"] = "verified release bytes"
        payloads = list(native_payloads(args.directory, system, arch))
        if len(payloads) != 2:
            raise ValueError("lifecycle requires exactly the two reviewed releases")
        # Short /tmp pathname keeps Unix sockets below native pathname limits.
        with tempfile.TemporaryDirectory(prefix="release-life-", dir="/tmp") as temporary:
            with homebrew_installation(payloads, system, arch, report, run) as installation:
                exercise(payloads, Path(temporary).resolve(), report, installation)
        report.update(success=True, stage="complete")
    except Exception as exc:
        report["error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
