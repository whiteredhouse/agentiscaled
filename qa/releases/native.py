"""Consumer tests on disposable native runners; no source builds or live nodes."""
import argparse
from contextlib import contextmanager
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import subprocess
import tempfile

from verify import PINS, download, native_payloads


def run(*command, env=None, timeout=300):
    print("+", " ".join(map(str, command)), flush=True)
    try:
        result = subprocess.run(list(map(str, command)), env=env, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)
    except subprocess.TimeoutExpired as exc:
        for output in (exc.stdout, exc.stderr):
            if output:
                print(output.decode(errors="replace") if isinstance(output, bytes) else output, flush=True)
        raise RuntimeError(f"command timed out after {timeout}s: {command[0]}") from exc
    print(result.stdout, flush=True)
    if result.stderr:
        print(result.stderr, flush=True)
    if result.returncode:
        raise RuntimeError(f"command failed ({result.returncode}): {command[0]}")
    return result.stdout.strip()


def binary_check(command, release, system, arch, digest, env=None):
    want = f'agentiscaled {release["version"]} (commit {release["commit"][:12]}, {system}/{arch})'
    got = run(command, "version", env=env)
    if got != want:
        raise ValueError(f"wrong native version: {got!r}; expected {want!r}")
    if hashlib.sha256(Path(command).read_bytes()).hexdigest() != digest:
        raise ValueError("installed binary differs from verified release archive")


def sentinels(paths):
    # Opaque test state, deliberately not claimed as a valid live node identity.
    for i, path in enumerate(paths):
        if path.exists() or path.is_symlink():
            raise ValueError("consumer fixture must not overwrite existing state")
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(f"release-upgrade-fixture-{i}\n".encode())
    return {path: path.read_bytes() for path in paths}


def unchanged(snapshot):
    if any(not path.is_file() or path.read_bytes() != data for path, data in snapshot.items()):
        raise ValueError("upgrade modified sentinel configuration/state/service files")


@contextmanager
def sentinel_files(paths):
    """Remove only files this fixture created, including after a failed upgrade."""
    owned = {}
    snapshot = {}
    try:
        for i, path in enumerate(paths):
            if path.exists() or path.is_symlink():
                raise ValueError("consumer fixture must not overwrite existing state")
            path.parent.mkdir(parents=True, exist_ok=True)
            contents = f"release-upgrade-fixture-{i}\n".encode()
            with path.open("xb") as stream:
                identity = os.fstat(stream.fileno())
                owned[path] = (identity.st_dev, identity.st_ino)
                stream.write(contents)
            snapshot[path] = contents
        yield snapshot
    finally:
        errors = []
        for path, identity in owned.items():
            try:
                current = path.lstat()
                if (current.st_dev, current.st_ino) != identity:
                    raise RuntimeError(f"sentinel ownership changed: {path}")
                path.unlink()
            except FileNotFoundError:
                pass
            except Exception as exc:
                errors.append(str(exc))
        if errors:
            raise RuntimeError("sentinel cleanup failed: " + "; ".join(errors))


@contextmanager
def cask_operations(brew, env):
    """Own only a previously absent cask and always clean an attempted install."""
    prefix = Path(run(brew, "--prefix", env=env).splitlines()[-1])
    cask = Path(run(brew, "--caskroom", env=env).splitlines()[-1]) / "agentiscaled"
    stable = prefix / "bin" / "agentiscaled"
    if cask.exists() or cask.is_symlink() or stable.exists() or stable.is_symlink():
        raise ValueError("consumer fixture must not overwrite an existing Homebrew installation")
    attempted = False

    def operate(operation):
        nonlocal attempted
        attempted = True
        run(brew, operation, "--cask", "whiteredhouse/tap/agentiscaled", env=env, timeout=1200)

    try:
        yield operate
    finally:
        if attempted:
            run(brew, "uninstall", "--cask", "whiteredhouse/tap/agentiscaled", env=env, timeout=1200)
            if cask.exists() or cask.is_symlink() or stable.exists() or stable.is_symlink():
                raise RuntimeError("Homebrew consumer cleanup left the cask or executable installed")


def consumer_environment(directory):
    env = os.environ.copy()
    # Disable PATH-based Go execution before any release binary is invoked.
    shim = directory / "disabled-go"
    shim.mkdir()
    go_log = directory / "go-invocations"
    (shim / "go").write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "$AGENTISCALE_QA_GO_LOG"\nexit 97\n')
    (shim / "go").chmod(0o755)
    env.update({"PATH": str(shim) + os.pathsep + env["PATH"], "AGENTISCALE_QA_GO_LOG": str(go_log),
                "GOROOT": str(directory / "no-go-toolchain"), "GOTOOLCHAIN": "local"})
    return env, go_log


def homebrew(payloads, system, arch, directory, env, go_log, report):
    env = env.copy()
    env.update({"HOMEBREW_NO_AUTO_UPDATE": "1", "HOMEBREW_NO_ANALYTICS": "1", "HOMEBREW_NO_ENV_HINTS": "1"})
    brew = shutil.which("brew")
    tooling = PINS["environment"]["homebrew"]
    latest = os.environ.get("AGENTISCALE_LATEST_HOMEBREW") == "true"
    report.update(mode="latest compatibility" if latest else "pinned baseline", installer_commit=tooling["installer_commit"])
    if not brew:
        # The immutable vendor bootstrap selects its own current brew; pin the
        # operational checkout immediately afterwards, before consumer checks.
        installer = directory / f'install-homebrew-{tooling["installer_commit"]}.sh'
        download(f'https://raw.githubusercontent.com/Homebrew/install/{tooling["installer_commit"]}/install.sh', installer, tooling["installer_sha256"])
        env["NONINTERACTIVE"] = "1"
        run("bash", installer, env=env, timeout=1200)
        brew = next((p for p in ("/home/linuxbrew/.linuxbrew/bin/brew", "/opt/homebrew/bin/brew", "/usr/local/bin/brew") if Path(p).exists()), None)
        if not brew:
            raise RuntimeError("Homebrew installer did not provide brew")
    brew_repository = Path(brew).resolve().parent.parent
    if latest:
        run(brew, "update", env=env, timeout=1200)
    else:
        run("git", "-C", brew_repository, "fetch", "--depth=1", "origin", tooling["commit"], env=env)
        run("git", "-C", brew_repository, "checkout", "--detach", tooling["commit"], env=env)
    report["commit"] = run("git", "-C", brew_repository, "rev-parse", "HEAD", env=env)
    if not latest and report["commit"] != tooling["commit"]:
        raise ValueError("Homebrew checkout differs from pinned baseline")
    brew_version = run(brew, "--version", env=env)
    report["brew"] = brew_version
    report["operations"] = []
    run(brew, "tap", "whiteredhouse/tap", env=env)
    tap = run(brew, "--repo", "whiteredhouse/tap", env=env).splitlines()[-1]
    run("git", "-C", tap, "fetch", "origin", env=env)
    home = Path.home()
    config_home = Path(env.get("XDG_CONFIG_HOME", str(home / ".config")))
    node = home / "Library/Application Support/agentiscale" if system == "darwin" else config_home / "agentiscale"
    service = home / "Library/LaunchAgents/net.agentiscale.qa-fixture.plist" if system == "darwin" else config_home / "systemd/user/agentiscaled-qa-fixture.service"
    with sentinel_files([node / "state-sentinel", node / "agentiscaled.json", service]) as snapshot:
        with cask_operations(brew, env) as operate:
            for i, (release, _, digest) in enumerate(payloads):
                run("git", "-C", tap, "checkout", "--detach", release["tap_commit"], env=env)
                operation = "install" if i == 0 else "upgrade"
                operate(operation)
                prefix = run(brew, "--prefix", env=env).splitlines()[-1]
                binary = Path(prefix) / "bin" / "agentiscaled"
                binary_check(binary, release, system, arch, digest, env=env)
                # Verify the active link points into this exact cask version, too.
                resolved = str(binary.resolve())
                if "/" + release["version"] + "/" not in resolved:
                    raise ValueError(f"brew link does not select reviewed version: {resolved}")
                for filename, key in (("LICENSE", "license_sha256"), ("NOTICE", "notice_sha256")):
                    expected = release.get(key, PINS[key])
                    if hashlib.sha256((binary.resolve().parent / filename).read_bytes()).hexdigest() != expected:
                        raise ValueError("Homebrew staged notices differ from the reviewed release")
                unchanged(snapshot)
                report["operations"].append({"version": release["version"], "operation": operation, "binary": resolved, "sentinels": "preserved"})
    if go_log.exists() and go_log.stat().st_size:
        raise ValueError("Homebrew consumer flow attempted to invoke Go")
    report["go"] = "PATH command disabled and not invoked"


def packages(payloads, arch, report):
    script = Path(__file__).with_name("package.sh").resolve()
    # Put only already-verified native packages into a read-only container mount.
    with tempfile.TemporaryDirectory(prefix="release-packages-") as work:
        root = Path(work)
        for _, folder, _ in payloads:
            for path in folder.iterdir():
                if path.name.endswith((".deb", ".rpm")):
                    shutil.copy2(path, root / path.name)
        before, after = payloads
        for kind, image in PINS["environment"]["package_images"].items():
            container = "agentiscale-release-" + kind + "-" + root.name
            arguments = []
            for release, _, digest in (before, after):
                arguments.extend((release["version"], release["commit"][:12], digest,
                                  release.get("license_sha256", PINS["license_sha256"]),
                                  release.get("notice_sha256", PINS["notice_sha256"])))
            try:
                run("docker", "run", "--rm", "--name", container, "--platform", "linux/" + arch,
                    "-v", f"{root}:/payload:ro", "-v", f"{script}:/checks/package.sh:ro", image,
                    "bash", "/checks/package.sh", kind, arch, *arguments, timeout=1800)
            except Exception:
                # A timed-out Docker client does not stop the daemon's container.
                try:
                    run("docker", "rm", "--force", container, timeout=60)
                except RuntimeError:
                    pass  # Keep the original failure; --rm may have removed it.
                raise
            digests = json.loads(run("docker", "image", "inspect", image, "--format", "{{json .RepoDigests}}"))
            if not any(value.endswith("@" + image.split("@", 1)[1]) for value in digests):
                raise ValueError("container image differs from pinned digest")
            report.append({"format": kind, "image": image, "image_digests": digests, "install_upgrade": "passed"})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--system", choices=("darwin", "linux"), required=True)
    parser.add_argument("--arch", choices=("arm64", "amd64"), required=True)
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("CI") != "true":
        raise SystemExit("consumer verification executes only on disposable CI runners")
    system = {"Darwin": "darwin", "Linux": "linux"}.get(platform.system())
    arch = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine())
    if (system, arch) != (args.system, args.arch):
        raise SystemExit("native runner does not match the declared platform")
    args.directory.mkdir(parents=True, exist_ok=True)
    report = {"success": False, "system": system, "arch": arch, "runner": platform.platform()}
    try:
        report["stage"] = "download/hash"
        payloads = native_payloads(args.directory, system, arch)
        env, go_log = consumer_environment(args.directory)
        report["stage"] = "archive execution"
        for release, folder, digest in payloads:
            binary_check(folder / "agentiscaled", release, system, arch, digest, env=env)
        report["archive_execution"] = "both releases passed"
        report["stage"] = "homebrew"
        report["homebrew"] = {}
        homebrew(payloads, system, arch, args.directory, env, go_log, report["homebrew"])
        if system == "linux":
            report["stage"] = "packages"
            report["packages"] = []
            packages(payloads, arch, report["packages"])
        report["success"] = True
        report["stage"] = "complete"
    except Exception as exc:
        report["error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
