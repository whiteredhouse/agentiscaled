"""Verify immutable public release bytes; never build or publish a release."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile
import time
import urllib.request


MAX_DOWNLOAD_BYTES = 200 * 1024 * 1024
DOWNLOAD_TIMEOUT_SECONDS = 120
PROVENANCE_TIMEOUT_SECONDS = 180


def load_pins(path=None):
    """Allow CI discovery to supply the same immutable-input manifest shape."""
    path = Path(path or os.environ.get("AGENTISCALE_RELEASE_PINS") or Path(__file__).with_name("pins.json"))
    with path.open("rb") as source:
        data = source.read(1024 * 1024 + 1)
    if len(data) > 1024 * 1024:
        raise ValueError("release pins manifest too large")
    pins = json.loads(data)
    for key in ("license_sha256", "notice_sha256"):
        if not re.fullmatch(r"[0-9a-f]{64}", pins[key]):
            raise ValueError(f"invalid {key}")
    if len(pins["releases"]) != 2:
        raise ValueError("release pins must contain the old and new release")
    for release in pins["releases"]:
        if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+", release["version"]):
            raise ValueError("invalid release version")
        for key, size in (("commit", 40), ("tap_commit", 40), ("manifest_sha256", 64)):
            if not re.fullmatch(r"[0-9a-f]{%d}" % size, release[key]):
                raise ValueError(f"release {key} must be immutable")
        for key in ("license_sha256", "notice_sha256"):
            if key in release and not re.fullmatch(r"[0-9a-f]{64}", release[key]):
                raise ValueError(f"invalid release {key}")
    if pins["releases"][0]["version"] == pins["releases"][1]["version"]:
        raise ValueError("old and new releases must differ")
    for key in ("commit", "installer_commit"):
        if not re.fullmatch(r"[0-9a-f]{40}", pins["environment"]["homebrew"][key]):
            raise ValueError(f"Homebrew {key} must be immutable")
    if not re.fullmatch(r"[0-9a-f]{64}", pins["environment"]["homebrew"]["installer_sha256"]):
        raise ValueError("invalid Homebrew installer_sha256")
    if set(pins["environment"]["package_images"]) != {"deb", "rpm"}:
        raise ValueError("package images must contain exactly deb and rpm")
    for kind in ("deb", "rpm"):
        if not re.fullmatch(r"[a-z0-9./:_-]+@sha256:[0-9a-f]{64}", pins["environment"]["package_images"][kind]):
            raise ValueError(f"{kind} image must use an immutable digest")
    return pins


PINS = load_pins()


def sha256(data):
    return hashlib.sha256(data).hexdigest()


def names(version):
    return {f"agentiscaled_{version}_{system}_{arch}.tar.gz"
            for system in ("darwin", "linux") for arch in ("arm64", "amd64")} | {
        f"agentiscaled_{version}_{arch}.deb" for arch in ("arm64", "amd64")} | {
        f"agentiscaled-{version}-1.{arch}.rpm" for arch in ("aarch64", "x86_64")}


def manifest(data, release):
    if sha256(data) != release["manifest_sha256"]:
        raise ValueError("checksum manifest differs from reviewed digest")
    out = {}
    for line in data.decode("ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.-]+)", line)
        if not match or match[2] in out:
            raise ValueError("malformed/duplicate checksum entry")
        out[match[2]] = match[1]
    if set(out) != names(release["version"]):
        raise ValueError("release asset inventory differs from expected eight packages")
    return out


def download(url, destination, expected=None):
    """No Authorization header or token is used, even on authenticated CI hosts."""
    destination = Path(destination)
    destination.parent.mkdir(parents=True, exist_ok=True)
    if not url.startswith("https://"):
        raise ValueError("release download requires HTTPS")
    if not destination.exists():
        for attempt in range(3):
            temporary = None
            try:
                request = urllib.request.Request(url, headers={"User-Agent": "agentiscale-release-verification"})
                deadline = time.monotonic() + DOWNLOAD_TIMEOUT_SECONDS
                with urllib.request.urlopen(request, timeout=30) as response:
                    if not response.url.startswith("https://"):
                        raise ValueError("release download redirected outside HTTPS")
                    length = response.headers.get("Content-Length")
                    if length is not None and (not length.isdecimal() or int(length) > MAX_DOWNLOAD_BYTES):
                        raise ValueError("invalid/oversized release Content-Length")
                    digest, size = hashlib.sha256(), 0
                    with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=destination.name + ".", suffix=".part", delete=False) as output:
                        temporary = Path(output.name)
                        while True:
                            if time.monotonic() >= deadline:
                                raise TimeoutError("release download exceeded total deadline")
                            # read1 returns after one buffered/raw read, allowing
                            # the deadline check even when the peer trickles bytes.
                            chunk = response.read1(64 * 1024)
                            if not chunk:
                                break
                            size += len(chunk)
                            if size > MAX_DOWNLOAD_BYTES:
                                raise ValueError("release asset too large")
                            digest.update(chunk)
                            output.write(chunk)
                        if length is not None and size != int(length):
                            raise ValueError("truncated release download")
                        if expected and digest.hexdigest() != expected:
                            raise ValueError(f"asset checksum mismatch: {destination.name}")
                        output.flush()
                        os.fsync(output.fileno())
                os.replace(temporary, destination)
                break
            except OSError:
                if attempt == 2:
                    raise
                time.sleep(2)
            finally:
                if temporary is not None:
                    temporary.unlink(missing_ok=True)
    with destination.open("rb") as source:
        data = source.read(MAX_DOWNLOAD_BYTES + 1)
    if len(data) > MAX_DOWNLOAD_BYTES:
        raise ValueError("cached release asset too large")
    if expected and sha256(data) != expected:
        raise ValueError(f"asset checksum mismatch: {destination.name}")
    return data


def fetch_manifest(release, directory):
    base = f'https://github.com/{PINS["repository"]}/releases/download/v{release["version"]}'
    path = Path(directory) / release["version"] / "checksums.txt"
    data = download(base + "/checksums.txt", path, release["manifest_sha256"])
    return base, path, manifest(data, release)


def archive(path, *, license_digest=None, notice_digest=None):
    wanted = {"agentiscaled", "LICENSE", "NOTICE", "README.md"}
    files = {}
    with tarfile.open(path, "r:gz") as tar:
        for entry in tar:
            if entry.name not in wanted or entry.name in files or not entry.isfile() or entry.size > 100 * 1024 * 1024:
                raise ValueError("unexpected, duplicate or unsafe archive entry")
            if entry.name == "agentiscaled" and entry.mode & 0o111 != 0o111:
                raise ValueError("archive binary lacks executable permissions")
            files[entry.name] = tar.extractfile(entry).read()
    if set(files) != wanted:
        raise ValueError("archive missing binary/license/notice/readme")
    if sha256(files["LICENSE"]) != (license_digest or PINS["license_sha256"]):
        raise ValueError("wrong Apache license")
    if sha256(files["NOTICE"]) != (notice_digest or PINS["notice_sha256"]):
        raise ValueError("wrong embedded-fork notice")
    return files


def provenance(path, release):
    command = ["gh", "attestation", "verify", str(path), "--repo", PINS["repository"],
               "--signer-workflow", PINS["repository"] + "/.github/workflows/release.yml",
               "--source-ref", "refs/tags/v" + release["version"],
               "--source-digest", release["commit"], "--format", "json"]
    # Transient GitHub/Sigstore network failures may be retried; a persistent
    # verification failure always fails the job and retains its diagnostics.
    for attempt in range(3):
        try:
            result = subprocess.run(command, text=True, capture_output=True, timeout=PROVENANCE_TIMEOUT_SECONDS)
        except subprocess.TimeoutExpired as exc:
            if attempt == 2:
                raise RuntimeError(f"provenance timed out for {path.name} after {PROVENANCE_TIMEOUT_SECONDS}s per attempt") from exc
            time.sleep(2)
            continue
        if result.returncode == 0:
            verified = json.loads(result.stdout)
            if not verified:
                raise ValueError("empty provenance verification")
            path.with_name(path.name + ".attestation.json").write_text(result.stdout)
            return
        if attempt == 2:
            raise RuntimeError(f"provenance failed for {path.name}: {result.stderr}")
        time.sleep(2)


def check_all(directory, report=None, status=None):
    if report is None:
        report = []
    if status is None:
        status = {}
    for release in PINS["releases"]:
        status.update(version=release["version"], asset="checksums.txt", stage="manifest")
        base, path, hashes = fetch_manifest(release, directory)
        status["stage"] = "provenance"
        provenance(path, release)
        for name, digest in sorted(hashes.items()):
            status.update(asset=name, stage="download/hash")
            destination = path.parent / name
            download(base + "/" + name, destination, digest)
            status["stage"] = "provenance"
            provenance(destination, release)
            if name.endswith(".tar.gz"):
                status["stage"] = "archive contents"
                archive(destination, license_digest=release.get("license_sha256"), notice_digest=release.get("notice_sha256"))
            report.append({"version": release["version"], "source_commit": release["commit"],
                           "asset": name, "sha256": digest, "provenance": "verified"})
        status.update(asset="agentiscaled.rb", stage="tap")
        tap = download(f'https://raw.githubusercontent.com/{PINS["tap_repository"]}/{release["tap_commit"]}/Casks/agentiscaled.rb', path.parent / "agentiscaled.rb").decode()
        if f'version "{release["version"]}"' not in tap:
            raise ValueError("tap version differs from release")
        if set(re.findall(r'sha256 "([0-9a-f]{64})"', tap)) != {digest for name, digest in hashes.items() if name.endswith(".tar.gz")}:
            raise ValueError("tap archive hashes differ from verified manifest")
    return report


def native_payloads(directory, system, arch):
    result = []
    for release in PINS["releases"]:
        base, path, hashes = fetch_manifest(release, directory)
        filename = f'agentiscaled_{release["version"]}_{system}_{arch}.tar.gz'
        download(base + "/" + filename, path.parent / filename, hashes[filename])
        files = archive(path.parent / filename, license_digest=release.get("license_sha256"), notice_digest=release.get("notice_sha256"))
        binary = path.parent / "agentiscaled"
        binary.write_bytes(files["agentiscaled"])
        binary.chmod(0o755)
        if system == "linux":
            for name in (f'agentiscaled_{release["version"]}_{arch}.deb',
                         f'agentiscaled-{release["version"]}-1.{"aarch64" if arch == "arm64" else "x86_64"}.rpm'):
                download(base + "/" + name, path.parent / name, hashes[name])
        result.append((release, path.parent, sha256(files["agentiscaled"])))
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--directory", type=Path, required=True)
    parser.add_argument("--report", type=Path, required=True)
    args = parser.parse_args()
    if os.environ.get("CI") != "true":
        raise SystemExit("release verification executes only in disposable CI")
    report = {"success": False, "assets": [], "active": {}, "native_coverage": {
        "required_separate_job": ["linux/amd64"],
        "optional_manual_jobs_unverified_automatically": ["darwin/arm64", "darwin/amd64", "linux/arm64"]}}
    try:
        report["gh"] = subprocess.check_output(["gh", "--version"], text=True, timeout=30).splitlines()[0]
        check_all(args.directory, report["assets"], report["active"])
        report["success"] = True
        report["active"]["stage"] = "complete"
    except Exception as exc:
        report["error"] = f"{type(exc).__name__}: {exc}"
        raise
    finally:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        for path in args.directory.rglob("*.attestation.json"):
            target = args.report.parent / "attestations" / path.relative_to(args.directory)
            target.parent.mkdir(parents=True, exist_ok=True)
            shutil.copy2(path, target)
        args.report.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
