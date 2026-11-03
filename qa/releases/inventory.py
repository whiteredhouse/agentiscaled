"""Separate the immutable upgrade baseline from the audited release catalog."""
import copy
import json
from pathlib import Path
import re


MAX_INPUT_BYTES = 1024 * 1024
MAX_CATALOG_RELEASES = 41  # At most the native suite's existing forty pairs.
ROOT = Path(__file__).resolve().parent
VERSION = re.compile(r"(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)")


def version(value):
    if not isinstance(value, str) or VERSION.fullmatch(value) is None:
        raise ValueError("invalid catalog semantic version")
    return tuple(map(int, value.split(".")))


def _object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate inventory object key")
        result[key] = value
    return result


def _read(path):
    path = Path(path)
    if path.is_symlink():
        raise ValueError("release inventory must not be a symlink")
    with path.open("rb") as source:
        data = source.read(MAX_INPUT_BYTES + 1)
    if len(data) > MAX_INPUT_BYTES:
        raise ValueError("release inventory exceeds input budget")
    return json.loads(data, object_pairs_hook=_object)


def _digest(value, length):
    return isinstance(value, str) and re.fullmatch(r"[0-9a-f]{%d}" % length, value) is not None


def release_metadata(release, *, require_audited=True):
    if not isinstance(release, dict):
        raise ValueError("catalog release must be an object")
    version(release.get("version"))
    if not _digest(release.get("commit"), 40):
        raise ValueError("catalog release commit must be immutable")
    status = release.get("status", "audited")
    if status == "pending-publication":
        if set(release) != {"version", "commit", "status"}:
            raise ValueError("pending release must not claim audited artifact metadata")
        if require_audited:
            raise ValueError("catalog release " + release["version"] + " is pending publication and audit")
        return copy.deepcopy(release)
    if status != "audited":
        raise ValueError("unknown catalog release status")
    required = {"version", "commit", "manifest_sha256", "tap_commit"}
    if not required <= set(release) or set(release) - required - {"status", "license_sha256", "notice_sha256"}:
        raise ValueError("audited catalog release metadata is incomplete or unknown")
    if not _digest(release["manifest_sha256"], 64) or not _digest(release["tap_commit"], 40):
        raise ValueError("audited catalog artifact inputs must be immutable")
    for key in ("license_sha256", "notice_sha256"):
        if key in release and not _digest(release[key], 64):
            raise ValueError("invalid catalog " + key)
    return {key: value for key, value in release.items() if key != "status"}


def validate_catalog(catalog, baseline):
    if not isinstance(catalog, dict) or set(catalog) != {"schema_version", "repository", "baseline_pair", "releases"}:
        raise ValueError("invalid release catalog shape")
    if type(catalog["schema_version"]) is not int or catalog["schema_version"] != 1:
        raise ValueError("unsupported release catalog schema")
    if not isinstance(baseline, dict) or len(baseline.get("releases", [])) != 2:
        raise ValueError("reviewed baseline must contain exactly two releases")
    expected = [release_metadata(release) for release in baseline["releases"]]
    if (not isinstance(catalog["repository"], str) or
            re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", catalog["repository"]) is None or
            catalog["repository"] != baseline.get("repository")):
        raise ValueError("catalog repository differs from reviewed baseline")
    if catalog["baseline_pair"] != [release["version"] for release in expected]:
        raise ValueError("catalog baseline selection differs from reviewed baseline")
    releases = catalog["releases"]
    if not isinstance(releases, list) or not 2 <= len(releases) <= MAX_CATALOG_RELEASES:
        raise ValueError("release catalog exceeds pair budget or omits baseline")
    entries = [release_metadata(release, require_audited=False) for release in releases]
    versions = [version(release["version"]) for release in entries]
    if versions != sorted(set(versions)):
        raise ValueError("catalog versions must be unique and ordered")
    if entries[:2] != expected:
        raise ValueError("catalog must preserve the complete reviewed baseline pair")
    return copy.deepcopy(catalog)


def load_catalog(path=None, *, baseline=None):
    return validate_catalog(_read(path or ROOT / "catalog.json"),
                            _read(ROOT / "pins.json") if baseline is None else baseline)


def catalog_releases(catalog):
    """Pending declarations never become audited just because an API sees them."""
    return [release_metadata(release) for release in catalog["releases"]]


def select_release(catalog, release_version, *, commit=None):
    selected = next((release for release in catalog["releases"] if release["version"] == release_version), None)
    if selected is None:
        raise ValueError("release is absent from the audited catalog")
    result = release_metadata(selected)
    if commit is not None and result["commit"] != commit:
        raise ValueError("release source differs from the audited catalog")
    return result


def required_pairs(catalog):
    releases = catalog_releases(catalog)
    return [(releases[index - 1], release) for index, release in enumerate(releases) if index]
