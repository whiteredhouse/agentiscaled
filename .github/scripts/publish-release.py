"""Publish an attested draft and update its Homebrew cask."""
import argparse
import base64
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys


TAP_FILE = "repos/whiteredhouse/homebrew-tap/contents/Casks/agentiscaled.rb"


def gh(*args, token=None):
    env = os.environ if token is None else dict(os.environ, GH_TOKEN=token)
    return json.loads(subprocess.check_output(["gh", *args], env=env, timeout=60))


def release_for_tag(repo, tag):
    pages = gh("api", f"repos/{repo}/releases", "--paginate", "--slurp")
    matches = [release for page in pages for release in page if release["tag_name"] == tag]
    if len(matches) > 1:
        raise ValueError(f"Multiple releases exist for {tag}")
    return matches[0] if matches else None


def attested_assets(repo, tag, manifest, bundle):
    verified = gh(
        "attestation", "verify", str(manifest), "--bundle", str(bundle),
        "--repo", repo, "--signer-workflow", f"{repo}/.github/workflows/release.yml",
        "--source-ref", f"refs/tags/{tag}", "--source-digest", os.environ["GITHUB_SHA"],
        "--format", "json",
    )
    if len(verified) != 1:
        raise ValueError("Expected one verified release attestation")
    subjects = verified[0]["verificationResult"]["statement"]["subject"]
    signed = {subject["name"]: subject["digest"]["sha256"] for subject in subjects}

    data = manifest.read_bytes()
    expected = {}
    for line in data.decode("ascii").splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.+-]+)", line)
        if not match or match[2] in expected:
            raise ValueError("Malformed or duplicate checksum entry")
        expected[match[2]] = match[1]
    for suffix, count in ((".tar.gz", 4), (".deb", 2), (".rpm", 2)):
        if sum(name.endswith(suffix) for name in expected) != count:
            raise ValueError(f"Expected {count} {suffix} packages in {manifest.name}")
    if len(expected) != 8:
        raise ValueError("Unexpected entries in checksum manifest")
    expected[manifest.name] = hashlib.sha256(data).hexdigest()
    if len(signed) != len(subjects) or signed != expected:
        raise ValueError("Signed subjects differ from the release checksum manifest")
    return {name: f"sha256:{digest}" for name, digest in signed.items()}


def publish(repo, tag, manifest, bundle):
    expected = attested_assets(repo, tag, manifest, bundle)
    release = release_for_tag(repo, tag)
    if not release:
        raise ValueError(f"No release exists for {tag}")
    endpoint = f"repos/{repo}/releases/{release['id']}"
    pages = gh("api", f"{endpoint}/assets", "--paginate", "--slurp")
    assets = [asset for page in pages for asset in page]
    uploaded = {asset["name"]: asset["digest"] for asset in assets if asset["state"] == "uploaded"}
    if len(assets) != len(expected) or uploaded != expected:
        changed = sorted(name for name in expected.keys() | uploaded.keys()
                         if expected.get(name) != uploaded.get(name))
        raise ValueError(f"Draft assets differ from attested assets. Missing or changed: {changed}")
    # A previous attempt may have published successfully before losing its
    # response. Recheck the same signed inventory before resuming.
    was_draft = release["draft"]
    if was_draft:
        flags = ["-f", "make_latest=false"] if release["prerelease"] else []
        release = gh("api", endpoint, "--method", "PATCH", "-F", "draft=false", *flags)
    if release["draft"] or release["tag_name"] != tag:
        raise ValueError(f"GitHub did not publish {tag}")
    print(f"Verified {len(expected)} assets for {tag}. " +
          ("Published the draft." if was_draft else "The release was already public."))
    if output := os.environ.get("GITHUB_OUTPUT"):
        with open(output, "a", encoding="utf-8") as stream:
            print(f"prerelease={str(release['prerelease']).lower()}", file=stream)


def cask_version(content):
    match = re.search(rb'^  version "(\d+)\.(\d+)\.(\d+)"$', content, re.MULTILINE)
    if not match:
        raise ValueError("Expected a stable version in the agentiscaled cask")
    return tuple(map(int, match.groups()))


def update_tap(repo, tag, cask):
    token = os.environ.get("HOMEBREW_TAP_TOKEN")
    if not token:
        print("HOMEBREW_TAP_TOKEN is absent. Skipping Homebrew.")
        return
    release = gh("api", f"repos/{repo}/releases/tags/{tag}")
    if release["draft"] or release["prerelease"] or release["tag_name"] != tag:
        raise ValueError(f"Expected a published stable release for {tag}")

    content = cask.read_bytes()
    version = cask_version(content)
    if tag != "v" + ".".join(map(str, version)):
        raise ValueError(f"Generated cask does not match {tag}")
    existing = gh("api", TAP_FILE, token=token)
    previous = base64.b64decode(existing["content"])
    if previous == content:
        print(f"Homebrew already serves {tag}")
        return
    if cask_version(previous) >= version:
        raise ValueError(f"Homebrew already has {tag} or a newer version with different content")

    # The blob SHA prevents a concurrent update from being silently overwritten.
    gh(
        "api", TAP_FILE, "--method", "PUT", "-f", f"sha={existing['sha']}",
        "-f", f"content={base64.b64encode(content).decode('ascii')}",
        "-f", f"message=Brew cask update for agentiscaled version {tag}",
        "-f", "committer[name]=agentiscale release",
        "-f", "committer[email]=noreply@agentiscale.net",
        token=token,
    )
    print(f"Updated Homebrew to {tag}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    commands.add_parser("check")
    publication = commands.add_parser("publish")
    publication.add_argument("manifest", type=Path)
    publication.add_argument("bundle", type=Path)
    tap = commands.add_parser("tap")
    tap.add_argument("cask", type=Path)
    args = parser.parse_args()
    repo = os.environ["GITHUB_REPOSITORY"]
    tag = os.environ["GITHUB_REF_NAME"]

    if args.command == "check":
        release = release_for_tag(repo, tag)
        if release and not release["draft"]:
            raise ValueError(f"{tag} is already public. Rerun only the failed publication or Homebrew job.")
    elif args.command == "publish":
        publish(repo, tag, args.manifest, args.bundle)
    else:
        update_tap(repo, tag, args.cask)


if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, KeyError, subprocess.SubprocessError) as error:
        # A failed gh call already writes its diagnostic to stderr. Avoid
        # printing subprocess arguments, which may contain a cask payload.
        if isinstance(error, subprocess.SubprocessError):
            sys.exit("GitHub CLI request failed")
        sys.exit(str(error))
