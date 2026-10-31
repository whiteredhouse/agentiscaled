"""Run published-release acceptance on disposable native CI runners."""
import json
import os
from pathlib import Path
import platform
import subprocess
import sys

import pytest

import discover
from lifecycle_platform import native_target


ROOT = Path(__file__).resolve().parents[2]
SCRIPTS = ROOT / "qa" / "releases"


def validated_pairs(plan):
    entries = plan["include"]
    if not entries:
        raise ValueError("empty release acceptance plan")
    if len(entries) > 40:
        raise ValueError("release acceptance pair budget exceeded; advance the reviewed baseline explicitly")
    selected = [tuple(release["version"] for release in entry["pins"]["releases"]) for entry in entries]
    if not any(entry["pins"]["releases"] == discover.BASE["releases"] for entry in entries):
        raise ValueError("release acceptance plan omits the exact reviewed baseline pair")
    if len(set(selected)) != len(selected):
        raise ValueError("duplicate release acceptance pair")
    return entries


def release_pairs():
    # Collection on a developer machine must never discover or download releases.
    automatic = os.environ.get("CI") == "true" and os.environ.get("RELEASE_DISCOVER") == "true"
    return validated_pairs(discover.plan(automatic, False))


def pair_id(entry):
    return "-to-".join(release["version"] for release in entry["pins"]["releases"])


def phase(script, arguments, directory, report, env):
    log = report.with_suffix(".log")
    command = [sys.executable, str(SCRIPTS / script), "--directory", str(directory),
               "--report", str(report), *arguments]
    try:
        with log.open("w") as output:
            subprocess.run(command, cwd=ROOT, env=env, stdout=output, stderr=subprocess.STDOUT, check=True)
    except subprocess.CalledProcessError:
        with log.open("rb") as output:
            output.seek(max(0, log.stat().st_size - 24000))
            print(output.read().decode(errors="replace"))
        raise
    details = json.loads(report.read_text())
    summary = {key: details[key] for key in (
        "assets", "homebrew", "packages", "observations", "upgrade_method", "mesh_evidence",
        "preservation", "crash_recovery", "rerun", "headless", "cleanup", "homebrew_cleanup") if key in details}
    print(f"{script} completed; report={report.relative_to(ROOT)}")
    print(json.dumps(summary, sort_keys=True)[:24000])


@pytest.mark.parametrize("entry", release_pairs(), ids=pair_id)
def test_native_release_acceptance(entry, tmp_path):
    if os.environ.get("CI") != "true":
        pytest.fail("native release acceptance runs only on disposable CI runners")
    apple_arm64 = platform.system() == "Darwin" and subprocess.check_output(
        ["/usr/sbin/sysctl", "-in", "hw.optional.arm64"], text=True).strip() == "1"
    system, arch = native_target(platform.system(), platform.machine(), apple_arm64=apple_arm64)
    if (system, arch) != (os.environ.get("RELEASE_SYSTEM"), os.environ.get("RELEASE_ARCH")):
        pytest.fail("actual native runner does not match the declared release platform")
    report_directory = ROOT / "reports" / "pairs" / pair_id(entry)
    report_directory.mkdir(parents=True, exist_ok=False)
    pins = report_directory / "pins.json"
    pins.write_text(json.dumps(entry["pins"], indent=2) + "\n")
    env = {**os.environ, "AGENTISCALE_RELEASE_PINS": str(pins)}
    print("immutable release inputs: " + json.dumps(entry["pins"], sort_keys=True))
    phase("verify.py", [], tmp_path, report_directory / "release-artifacts.json", env)
    phase("native.py", ["--system", system, "--arch", arch], tmp_path,
          report_directory / f"release-{system}-{arch}.json", env)
    phase("lifecycle.py", [], tmp_path, report_directory / f"lifecycle-{system}-{arch}.json", env)
