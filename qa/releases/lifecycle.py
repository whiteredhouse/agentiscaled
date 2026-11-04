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
import urllib.error

from verify import PINS, download, native_payloads
from lifecycle_platform import (DarwinSupervisor, LinuxSupervisor, native_target, verified_image)
from lifecycle_install import homebrew_installation


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def run(*command, env=None, cwd=None, timeout=90, check=True, captured=None):
    print("+", " ".join(map(str, command)), flush=True)
    result = subprocess.run(list(map(str, command)), env=env, cwd=cwd,
                            capture_output=True, text=True, timeout=timeout, stdin=subprocess.DEVNULL)
    if captured is not None:
        captured.extend((result.stdout, result.stderr))
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


def fixture_post(base, path, value=None, expected=200):
    request = urllib.request.Request(base + path, data=json.dumps(value or {}).encode(),
                                     headers={"Content-Type": "application/json"}, method="POST")
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(request, timeout=8) as response:
            if response.status != expected:
                raise ValueError(f"fixture POST {path} returned unexpected status")
            return json.load(response)
    except urllib.error.HTTPError as exc:
        exc.close()
        if exc.code != expected:
            raise ValueError(f"fixture POST {path}: HTTP {exc.code}") from exc
        return None


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
    shutil.copyfile(fixtures / "lifecycle_mesh_test.go", command / "main_test.go")
    alias = mesh_source / "export/standalone"
    alias.mkdir()
    shutil.copyfile(fixtures / "standalone_alias.go", alias / "standalone.go")
    output = root / "mesh-companion"
    run("go", "test", "-mod=readonly", "./cmd/qa-lifecycle", cwd=mesh_source, timeout=600)
    # Build exactly one companion command, never ./cmd/agentiscaled or ./....
    run("go", "build", "-mod=readonly", "-o", output, "./cmd/qa-lifecycle", cwd=mesh_source, timeout=600)
    report["companion"] = {"source_commit": commit, "source_archive_sha256": digest(archive),
                           "command": "go build -mod=readonly -o mesh-companion ./cmd/qa-lifecycle",
                           "protocol_tests": "go test -mod=readonly ./cmd/qa-lifecycle",
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


def wait_recovered(supervisor, previous, timeout=90):
    # Native managers briefly report a stopped/restarting service after SIGKILL.
    # Wait for their replacement process, never issue start/restart ourselves.
    deadline = time.monotonic() + timeout
    while True:
        try:
            current = supervisor.identity()
            if current["pid"] != previous["pid"]:
                return current
        except (OSError, ValueError, RuntimeError):
            pass
        if time.monotonic() >= deadline:
            raise RuntimeError(f"native supervisor did not automatically recover within {timeout} seconds")
        time.sleep(0.2)


def check_status(stable, data, env, supervisor, settings, connected, captured=None):
    capture = {} if captured is None else {"captured": captured}
    output = run(stable, "status", "-data-dir", data, env=env, **capture)
    fields = {}
    for line in output.splitlines():
        key, separator, value = line.partition(":")
        if separator:
            if key in fields:
                raise ValueError("status returned a duplicate field")
            fields[key] = value.strip()
    if supervisor.kind == "systemd user service":
        prefix, state = "systemd user unit", "installed, active"
    else:
        prefix, state = "launchd agent", "installed, running"
    expected_service = f'{prefix} {supervisor.name}, {state if connected else "not installed"}'
    expected = {"Node": f'{settings["peer_id"]} ({settings["machine_name"]})',
                "Network": settings["network_id"], "Mesh": settings["mesh_url"],
                "Service": expected_service}
    if any(fields.get(key) != value for key, value in expected.items()):
        raise ValueError("status differs from the enrolled node's identity or native service state")
    if connected and fields.get("Mesh connection") != "connected":
        raise ValueError("status did not report the connected mesh")
    if not connected and fields.get("Mesh connection") != "node not running":
        raise ValueError("status did not report the stopped node")
    return fields


def primary_unchanged(supervisor, previous, data, peer, preserved):
    supervisor.unchanged_process(previous)
    mesh = wait_ready(data)
    supervisor.unchanged_process(previous)
    if mesh["peer_id"] != peer:
        raise ValueError("secondary lifecycle changed the primary node's peer identity")
    if any(path.read_bytes() != contents for path, contents in preserved.items()):
        raise ValueError("secondary lifecycle changed the primary settings or service")
    return {"pid": previous["pid"], "start": previous["start"], "peer_id": peer, "readyz": 200}


def check_headless_evidence(evidence, baseline, primary_peer, secondary_peer):
    if evidence["signin_locked"] is not True or evidence["enrollment_locked"] is not True:
        raise ValueError("headless authentication/enrollment controls were not locked")
    if evidence["enrollments"] != [primary_peer] or evidence["join_code_peers"] != [secondary_peer]:
        raise ValueError("headless lifecycle signed in or enrolled an unexpected node")
    for key in ("oauth_device_requests", "oauth_token_requests", "signin_enrollment_requests"):
        if baseline[key] != 1 or evidence[key] != baseline[key]:
            raise ValueError("headless lifecycle attempted OAuth or signed-in enrollment")
    for key in ("denied_auth_requests", "denied_join_requests", "unexpected_requests"):
        if evidence[key] != 0:
            raise ValueError("headless lifecycle made an unexpected authentication/join request")
    if evidence["rejected_join_requests"] != 2:
        raise ValueError("invalid and reused code controls did not both run")
    if evidence["join_requests"] != [{"peer_id": secondary_peer, "machine_name": "qa-headless"}]:
        raise ValueError("headless join request differs from the running node")


def secret_absent(secret, roots, outputs):
    token = secret.encode()
    if any(secret in output for output in outputs):
        raise ValueError("join code leaked into product output")
    for root in roots:
        for path in root.rglob("*"):
            if path.is_file() and token in path.read_bytes():
                raise ValueError("join code persisted in node data or fixture HOME")


def exercise_headless(fixture, root, home, env, primary, primary_peer, primary_identity,
                      preserved, secondary, image, release, report):
    data, stable, control = secondary.data, secondary.stable, fixture["control"]
    details = {"observations": [], "primary_isolation": []}
    report["headless"] = details
    previous_primary = report["observations"][-1]

    def isolated(stage):
        proof = primary_unchanged(primary, previous_primary, primary.data, primary_peer, preserved)
        details["primary_isolation"].append(dict(proof, stage=stage))

    baseline = http_json(control + "/evidence")
    fixture_post(control, "/lock-signin")
    request = {"code": "invalid-fixture-code", "peer_id": primary_peer, "machine_name": "qa-headless"}
    fixture_post(control, "/v1/join", request, expected=403)
    # The caller-owned input and private fixture descriptor live outside HOME
    # and both node data directories; neither is node persistence.
    code_file = root / "headless-code"
    with os.fdopen(os.open(code_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as stream:
        stream.write(fixture["join_code"] + "\n")
    outputs = []
    # Every up uses the local forbidden-auth fixture even when joined, so an
    # identity regression cannot fall back to production sign-in unnoticed.
    args = ["up", "-data-dir", data, "-clients", "none", "-y", "-control", control,
            "-auth-issuer", control + "/", "-auth-client-id", "ci-client", "-auth-audience", "ci-control"]
    secondary.prepare()
    since_us = time.time_ns() // 1000
    run(stable, *args, "-join-code-path", code_file, "-hostname", "qa-headless", env=env, timeout=150, captured=outputs)
    peer = running(secondary, image, data, release, details, since_us)
    if peer == primary_peer or secondary.name == primary.name or secondary.path == primary.path:
        raise ValueError("headless node did not get an independent identity/service")
    settings_path = data / "agentiscaled.json"
    settings = json.loads(settings_path.read_text())
    if settings["bind_addr"] != "" or settings["listen"] != "/ip4/0.0.0.0/udp/0/quic-v1,/ip4/0.0.0.0/tcp/0":
        raise ValueError("secondary up did not choose socket-only/free-port defaults")
    details["joined_status"] = check_status(stable, data, env, secondary, settings, True, captured=outputs)
    isolated("join-code up")
    request.update(code=fixture["join_code"], peer_id=peer)
    fixture_post(control, "/v1/join", request, expected=403)
    fixture_post(control, "/lock-enrollment")
    previous = details["observations"][-1]
    run(stable, "down", "-data-dir", data, env=env, captured=outputs)
    secondary.assert_removed(previous)
    identity = http_json(control + "/inspect-secondary")
    if (identity["peer_id"] != peer or identity["mesh_url"] != fixture["mesh_url"] or
            identity["key_sha256"] == primary_identity["key_sha256"]):
        raise ValueError("headless persisted identity differs from its independent running node")
    details["identity_before"] = identity
    details["stopped_status"] = check_status(stable, data, env, secondary, settings, False, captured=outputs)
    isolated("headless down")
    code_file.unlink()
    secondary.prepare()
    since_us = time.time_ns() // 1000
    run(stable, *args, env=env, timeout=150, captured=outputs)
    if running(secondary, image, data, release, details, since_us, previous) != peer:
        raise ValueError("headless up after deleting code changed peer identity")
    if json.loads(settings_path.read_text()) != settings:
        raise ValueError("headless resumed up changed saved settings")
    details["resumed_status"] = check_status(stable, data, env, secondary, settings, True, captured=outputs)
    isolated("headless resume without code")
    previous = details["observations"][-1]
    since_us = time.time_ns() // 1000
    updated_level = "debug" if settings["log_level"] != "debug" else "info"
    run(stable, *args, "-log-level", updated_level, env=env, timeout=150, captured=outputs)
    if running(secondary, image, data, release, details, since_us, previous) != peer:
        raise ValueError("running up option update changed headless peer identity")
    updated = dict(settings, log_level=updated_level)
    if json.loads(settings_path.read_text()) != updated:
        raise ValueError("running up changed settings beyond the requested log level")
    details["updated_status"] = check_status(stable, data, env, secondary, updated, True, captured=outputs)
    isolated("running headless up option update")
    run(stable, "down", "-data-dir", data, env=env, captured=outputs)
    secondary.assert_removed(details["observations"][-1])
    if http_json(control + "/inspect-secondary") != identity or json.loads(settings_path.read_text()) != updated:
        raise ValueError("headless resume/update/down changed key, credential or saved options")
    isolated("final headless down")
    evidence = http_json(control + "/evidence")
    check_headless_evidence(evidence, baseline, primary_peer, peer)
    if any(evidence["router_peer_id"] not in item["mesh"]["attached_routers"] for item in details["observations"]):
        raise ValueError("headless node was not attached to the real router on every start")
    secret_absent(fixture["join_code"], (home, primary.data, data), outputs)
    details.update(identity_preserved=True, options_updated=True, code_removed=True,
                   service_removed=True, independent_services=True, authentication_evidence=evidence)


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
    secondary_data = root / "headless"
    secondary_data.mkdir(mode=0o700)
    stable = installation.stable
    supervisor_type = LinuxSupervisor if report["system"] == "linux" else DarwinSupervisor
    supervisor = supervisor_type(env, home, data, stable, run)
    secondary = supervisor_type(env, home, secondary_data, stable, run)
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
                                        "--secondary-node-data", str(secondary_data),
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
        report["stage"] = "automatic native recovery after a supervised node crash"
        previous = report["observations"][-1]
        started = time.monotonic()
        since_us = time.time_ns() // 1000
        report["crash_recovery"] = dict(supervisor.crash(previous), previous_pid=previous["pid"],
                                        previous_start=previous["start"], manual_start=False)
        wait_recovered(supervisor, previous)
        if running(supervisor, new_image, data, new[0], report, since_us, previous) != old_peer:
            raise ValueError("automatic crash recovery changed the enrolled peer identity")
        recovered = report["observations"][-1]
        report["crash_recovery"].update(recovered_pid=recovered["pid"], recovered_start=recovered["start"],
                                         readyz=200, elapsed_seconds=round(time.monotonic() - started, 3))
        supervisor.stop()
        identity_after = http_json(fixture["control"] + "/inspect")
        report["identity_after"] = identity_after
        if identity_before != identity_after:
            raise ValueError("upgrade changed persisted enrolled key or identity credential")
        for path, contents in preserved.items():
            if path.read_bytes() != contents:
                raise ValueError(f"upgrade changed persistent settings/service: {path.name}")
        report["stage"] = "product status and down/up with the enrolled identity"
        since_us = time.time_ns() // 1000
        supervisor.start()
        if running(supervisor, new_image, data, new[0], report, since_us, recovered) != old_peer:
            raise ValueError("service restart before down changed the enrolled peer identity")
        saved_settings = json.loads(settings.read_text())
        report["product_lifecycle"] = {"running_status": check_status(stable, data, env, supervisor, saved_settings, True)}
        previous = report["observations"][-1]
        supervisor.unchanged_process(previous)
        run(stable, "down", "-data-dir", data, env=env)
        supervisor.assert_removed(previous)
        if http_json(fixture["control"] + "/inspect") != identity_before or settings.read_bytes() != preserved[settings]:
            raise ValueError("product down changed the enrolled identity or settings")
        report["product_lifecycle"]["stopped_status"] = check_status(stable, data, env, supervisor, saved_settings, False)
        # Linux's real user manager belongs to the CI account, while this node's
        # config lives in an isolated HOME. Restore only its discovery link;
        # the published up command must generate/start the actual service again.
        supervisor.prepare()
        since_us = time.time_ns() // 1000
        run(stable, "up", "-data-dir", data, "-clients", "none", "-y",
            "-auth-issuer", fixture["control"] + "/", "-auth-client-id", "ci-client",
            "-auth-audience", "ci-control", env=env, timeout=150)
        if running(supervisor, new_image, data, new[0], report, since_us, previous) != old_peer:
            raise ValueError("product up after down changed the enrolled peer identity")
        if json.loads(settings.read_text()) != saved_settings:
            raise ValueError("product up after down changed the saved options")
        report["product_lifecycle"]["resumed_status"] = check_status(stable, data, env, supervisor, saved_settings, True)
        report["stage"] = "native headless join-code, running option update and independent services"
        primary_preserved = {path: path.read_bytes() for path in preserved}
        exercise_headless(fixture, root, home, env, supervisor, old_peer, identity_before,
                          primary_preserved, secondary, new_image, new[0], report)
        supervisor.stop()
        if http_json(fixture["control"] + "/inspect") != identity_before:
            raise ValueError("product down/up changed the persisted key or credential")
        report["product_lifecycle"].update(identity_preserved=True, settings_preserved=True, service_removed=True)
        evidence = http_json(fixture["control"] + "/evidence")
        report["mesh_evidence"] = evidence
        if evidence["enrollments"] != [old_peer]:
            raise ValueError("restart/upgrade/crash/down/up unexpectedly enrolled a fresh node")
        if any(evidence["router_peer_id"] not in item["mesh"]["attached_routers"] for item in report["observations"]):
            raise ValueError("node was not attached to the real fixture router on every start")
        report["service_unit"] = supervisor.name
        report["service"] = {"kind": supervisor.kind, "name": supervisor.name, "generated_file": str(supervisor.path)}
        report["preservation"] = "real enrolled key, credential, peer ID, settings and service unit preserved"
    finally:
        # Always stop the actual service before deleting its private database.
        # Cleanup errors fail the run: a claimed success cannot leave a node up.
        cleanup_errors = []
        report["service_cleanup"] = {}
        for name, service in (("headless", secondary), ("primary", supervisor)):
            try:
                errors = service.cleanup()
            except Exception as exc:
                errors = [str(exc)]
            report["service_cleanup"][name] = "passed" if not errors else errors
            cleanup_errors.extend(f"{name}: {error}" for error in errors)
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
        secondary_log = secondary_data / "agentiscaled.log"
        if secondary_log.exists():
            report["headless_log_tail"] = secondary_log.read_text(errors="replace")[-16000:]
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
              "observations": [], "scope": f"native {system}/{arch} real {supervisor_kind}; enrolled upgrade and headless services"}
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
