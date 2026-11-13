"""Exercise release publication with GitHub CLI calls replaced by a local fake."""
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

import pytest


SCRIPT = Path(__file__).resolve().parents[2] / ".github/scripts/publish-release.py"
TAG = "v1.2.3"
CASK = 'cask "agentiscaled" do\n  version "1.2.3"\nend\n'
PACKAGES = [
    f"agentiscaled_1.2.3_{system}_{arch}.tar.gz"
    for system in ("darwin", "linux") for arch in ("arm64", "amd64")
] + [
    "agentiscaled_1.2.3_amd64.deb", "agentiscaled_1.2.3_arm64.deb",
    "agentiscaled-1.2.3-1.aarch64.rpm", "agentiscaled-1.2.3-1.x86_64.rpm",
]
DIGEST = "a" * 64


@pytest.fixture
def publisher(tmp_path):
    manifest = tmp_path / "checksums.txt"
    manifest.write_text("".join(f"{DIGEST}  {name}\n" for name in PACKAGES))
    subjects = {name: DIGEST for name in PACKAGES}
    subjects[manifest.name] = hashlib.sha256(manifest.read_bytes()).hexdigest()
    assets = [
        {"name": name, "state": "uploaded", "digest": f"sha256:{digest}"}
        for name, digest in subjects.items()
    ]
    state = {
        "releases": [{"id": 42, "tag_name": TAG, "draft": True, "prerelease": False}],
        "assets": assets,
        "statement": {"subject": [
            {"name": name, "digest": {"sha256": digest}} for name, digest in subjects.items()
        ]},
        "cask": CASK.replace("1.2.3", "1.2.2"),
    }
    cask = tmp_path / "agentiscaled.rb"
    cask.write_text(CASK)
    gh = tmp_path / "gh"
    gh.write_text(f"#!{sys.executable}\n" + """\
import base64
import json
import os
from pathlib import Path
import sys

state = json.loads(Path(os.environ["FAKE_GH_STATE"]).read_text())
args = sys.argv[1:]
with Path(os.environ["FAKE_GH_LOG"]).open("a") as log:
    log.write(json.dumps(args) + "\\n")

if args[:2] == ["attestation", "verify"]:
    if state.get("bad_attestation"):
        sys.exit(1)
    result = [{"verificationResult": {"statement": state["statement"]}}]
elif args[:2] == ["api", "repos/whiteredhouse/agentiscaled/releases"]:
    result = [state["releases"]]
elif args[:2] == ["api", "repos/whiteredhouse/agentiscaled/releases/42/assets"]:
    result = [state["assets"]]
elif args[:2] == ["api", "repos/whiteredhouse/agentiscaled/releases/42"]:
    result = {**state["releases"][0], "draft": False}
elif args[:2] == ["api", "repos/whiteredhouse/agentiscaled/releases/tags/v1.2.3"]:
    result = state["releases"][0]
elif args[:2] == ["api", "repos/whiteredhouse/homebrew-tap/contents/Casks/agentiscaled.rb"]:
    if "PUT" in args:
        if state.get("tap_conflict"):
            sys.exit(1)
        result = {}
    else:
        result = {"sha": "previous-blob", "content": base64.b64encode(state["cask"].encode()).decode()}
else:
    sys.exit(f"unexpected gh call: {args}")
print(json.dumps(result))
""")
    gh.chmod(0o755)
    state_path = tmp_path / "state.json"
    log_path = tmp_path / "log.jsonl"
    env = dict(os.environ, PATH=f"{tmp_path}:{os.environ['PATH']}", FAKE_GH_STATE=str(state_path),
               FAKE_GH_LOG=str(log_path), GITHUB_REPOSITORY="whiteredhouse/agentiscaled",
               GITHUB_REF_NAME=TAG, GITHUB_SHA="1" * 40, HOMEBREW_TAP_TOKEN="test-tap-token")

    def run(command):
        state_path.write_text(json.dumps(state))
        log_path.write_text("")
        args = {"publish": [str(manifest), "bundle.json"], "tap": [str(cask)]}
        result = subprocess.run([sys.executable, SCRIPT, command, *args.get(command, [])],
                                env=env, text=True, capture_output=True)
        calls = [json.loads(line) for line in log_path.read_text().splitlines()]
        return result, calls

    return state, run


def test_publish_checks_provenance_and_all_draft_assets_before_undrafting(publisher):
    _, run = publisher
    result, calls = run("publish")
    assert result.returncode == 0, result.stderr
    assert len(calls) == 4
    verify_call, release_call, asset_call, update_call = calls
    assert verify_call[:2] == ["attestation", "verify"]
    assert "refs/tags/v1.2.3" in verify_call
    assert "1" * 40 in verify_call
    assert release_call[1].endswith("/releases")
    assert asset_call[1].endswith("/releases/42/assets")
    assert update_call[1].endswith("/releases/42")
    assert "PATCH" in update_call and "draft=false" in update_call


@pytest.mark.parametrize("failure", ["attestation", "signed-digest", "draft-digest", "missing", "extra", "not-uploaded"])
def test_bad_provenance_or_assets_prevent_publication(publisher, failure):
    state, run = publisher
    if failure == "attestation":
        state["bad_attestation"] = True
    elif failure == "signed-digest":
        state["statement"]["subject"][0]["digest"]["sha256"] = "b" * 64
    elif failure == "draft-digest":
        state["assets"][0]["digest"] = "sha256:" + "b" * 64
    elif failure == "missing":
        state["assets"].pop()
    elif failure == "extra":
        state["assets"].append({"name": "unknown", "state": "uploaded", "digest": "sha256:" + DIGEST})
    else:
        state["assets"][0]["state"] = "starter"
    result, calls = run("publish")
    assert result.returncode != 0
    assert calls and all("PATCH" not in call and "PUT" not in call for call in calls)


def test_existing_public_release_blocks_goreleaser_before_staging(publisher):
    state, run = publisher
    state["releases"][0]["draft"] = False
    result, calls = run("check")
    assert result.returncode != 0
    assert len(calls) == 1 and calls[0][1].endswith("/releases")


def test_publication_retry_reuses_public_release_only_if_assets_match(publisher):
    state, run = publisher
    state["releases"][0]["draft"] = False
    result, calls = run("publish")
    assert result.returncode == 0, result.stderr
    assert len(calls) == 3 and all("PATCH" not in call for call in calls)

    state["assets"][0]["digest"] = "sha256:" + "b" * 64
    result, calls = run("publish")
    assert result.returncode != 0
    assert len(calls) == 3 and all("PATCH" not in call for call in calls)


def test_tap_update_requires_public_release_and_reuses_cask_on_retry(publisher):
    state, run = publisher
    result, calls = run("tap")
    assert result.returncode != 0
    assert len(calls) == 1 and all("PUT" not in call for call in calls)

    state["releases"][0]["draft"] = False
    state["tap_conflict"] = True
    result, calls = run("tap")
    assert result.returncode != 0
    failed_update = calls[-1]
    assert "PUT" in failed_update

    state["tap_conflict"] = False
    result, calls = run("tap")
    assert result.returncode == 0, result.stderr
    assert calls[-1] == failed_update and "sha=previous-blob" in calls[-1]
    assert all("PATCH" not in call for call in calls)

    state["cask"] = CASK
    result, calls = run("tap")
    assert result.returncode == 0, result.stderr
    assert all("PUT" not in call for call in calls)


def test_tap_retry_cannot_overwrite_a_newer_cask(publisher):
    state, run = publisher
    state["releases"][0]["draft"] = False
    state["cask"] = CASK.replace("1.2.3", "1.10.0")
    result, calls = run("tap")
    assert result.returncode != 0
    assert calls and all("PUT" not in call for call in calls)


def test_prerelease_cannot_update_the_tap(publisher):
    state, run = publisher
    state["releases"][0].update(draft=False, prerelease=True)
    result, calls = run("tap")
    assert result.returncode != 0
    assert len(calls) == 1 and all("PUT" not in call for call in calls)
