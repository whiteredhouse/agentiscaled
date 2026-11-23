"""Install and launch the reviewed real desktop app on a fresh disposable Mac.

This is a prerequisite probe. It never signs in, invokes desktop tools, or emits
an acceptance observation. Only the allowlisted JSON report leaves the runner.
"""
import argparse
import ctypes
import errno
import hashlib
import json
import math
import os
from pathlib import Path, PurePosixPath
import platform
import plistlib
import pwd
import re
import shutil
import signal
import stat
import subprocess
import tempfile
import time
import unicodedata
import urllib.parse
import urllib.request
import zipfile


MAX_DOWNLOAD = 512 * 1024 * 1024
MAX_EXPANDED = 2 * 1024 * 1024 * 1024
MAX_MEMBERS = 20000
# Chromium's ordinary assistive-technology switch exposes renderer AX without
# changing the bundle, authentication, macOS permissions, or sandbox policy.
# https://github.com/chromium/chromium/blob/main/ui/accessibility/accessibility_switches.cc
RENDERER_ACCESSIBILITY_ARGUMENT = "--force-renderer-accessibility"
UI_STATUSES = {"passed", "blocked", "failed"}
UI_REASONS = {
    "verified", "not_attempted", "accessibility_not_trusted", "identity_changed", "deadline_exceeded",
    "no_owned_window", "no_unique_window", "window_not_settable", "window_state_unknown", "window_already_minimized",
    "minimize_write_failed", "minimize_readback_failed", "no_web_area", "no_original_focus",
    "no_distinct_focus_candidate", "focus_not_settable", "focus_write_failed", "focus_readback_failed", "restore_failed",
}
AX_RESULTS = {
    "success", "failure", "illegal_argument", "invalid_ui_element", "invalid_ui_element_observer", "cannot_complete",
    "attribute_unsupported", "action_unsupported", "notification_unsupported", "not_implemented",
    "notification_already_registered", "notification_not_registered", "api_disabled", "no_value",
    "parameterized_attribute_unsupported", "not_enough_precision", "unknown_ax_error", "not_attempted",
    "already_enabled", "unchanged", "original_unknown",
}
UI_BARRIERS = {
    "invalid_arguments", "identity_changed", "accessibility_not_trusted", "manual_accessibility_unavailable",
    "manual_accessibility_restore_failed", "inspection_incomplete", "native_window_control_blocked",
    "native_window_control_failed", "renderer_control_blocked", "renderer_control_failed", "deadline_exceeded",
    "internal_failure",
}
UI_ROLES = ("window", "web_area", "button", "text_field", "text_area", "static_text", "group",
            "scroll_area", "menu", "menu_item", "menu_bar", "unknown", "other")


class PreflightError(RuntimeError):
    """A fixed, non-sensitive failure code safe to retain."""


def run(*command, source=None, timeout=30):
    try:
        result = subprocess.run(list(map(str, command)), input=source, text=True,
                                capture_output=True, timeout=timeout, stdin=None if source else subprocess.DEVNULL)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise PreflightError("command_unavailable_or_timed_out") from exc
    if result.returncode:
        raise PreflightError("command_failed")
    return result.stdout, result.stderr


def jxa(source, *arguments, timeout=30):
    output, _ = run("/usr/bin/osascript", "-l", "JavaScript", "-", *arguments, source=source, timeout=timeout)
    try:
        value = json.loads(output)
    except (ValueError, TypeError) as exc:
        raise PreflightError("invalid_native_probe_result") from exc
    if not isinstance(value, dict):
        raise PreflightError("invalid_native_probe_result")
    return value


INVENTORY = r'''
ObjC.import('AppKit');
function run(argv) {
  const apps = $.NSRunningApplication.runningApplicationsWithBundleIdentifier($(argv[0]));
  return JSON.stringify({home: ObjC.unwrap($.NSHomeDirectory()), running: Number(apps.count)});
}
'''

LAUNCH = r'''
ObjC.import('AppKit');
function run(argv) {
  const error = Ref();
  const configuration = $.NSDictionary.dictionaryWithObjectForKey(
    $(argv.slice(1)), $.NSWorkspaceLaunchConfigurationArguments);
  const app = $.NSWorkspace.sharedWorkspace.launchApplicationAtURLOptionsConfigurationError(
    $.NSURL.fileURLWithPath($(argv[0])),
    $.NSWorkspaceLaunchNewInstance | $.NSWorkspaceLaunchWithoutAddingToRecents,
    configuration, error);
  if (!app || app.isNil()) throw Error('launch_failed');
  return JSON.stringify({pid: Number(app.processIdentifier),
    bundle_id: ObjC.unwrap(app.bundleIdentifier), bundle_path: ObjC.unwrap(app.bundleURL.path),
    executable_path: ObjC.unwrap(app.executableURL.path),
    launch_time: Number(app.launchDate.timeIntervalSince1970)});
}
'''


def load_pins(path):
    value = json.loads(path.read_text())
    required = {"version", "url", "sha256", "bundle_id", "publisher", "team_id", "cask_source_revision"}
    if not isinstance(value, dict) or set(value) != required:
        raise PreflightError("invalid_pin_fields")
    if not all(isinstance(v, str) and v for v in value.values()):
        raise PreflightError("invalid_pin_values")
    url = urllib.parse.urlsplit(value["url"])
    if (not re.fullmatch(r"[0-9]+(?:\.[0-9]+)+", value["version"]) or
            not re.fullmatch(r"[a-f0-9]{64}", value["sha256"]) or
            not re.fullmatch(r"[A-Z0-9]{10}", value["team_id"]) or
            not re.fullmatch(r"[a-f0-9]{40}", value["cask_source_revision"]) or
            url.scheme != "https" or url.netloc != "downloads.claude.ai" or url.query or url.fragment or
            not url.path.startswith("/releases/darwin/universal/" + value["version"] + "/") or
            not url.path.endswith(".zip") or value["bundle_id"] != "com.anthropic.claudefordesktop"):
        raise PreflightError("invalid_reviewed_pin")
    return value


def digest(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def download(pins, destination):
    total = 0
    with urllib.request.urlopen(pins["url"], timeout=60) as response, destination.open("xb") as stream:
        if urllib.parse.urlsplit(response.geturl()).scheme != "https":
            raise PreflightError("insecure_download_redirect")
        while block := response.read(1024 * 1024):
            total += len(block)
            if total > MAX_DOWNLOAD:
                raise PreflightError("download_budget_exceeded")
            stream.write(block)
    if digest(destination) != pins["sha256"]:
        raise PreflightError("archive_digest_mismatch")
    return total


def safe_name(name):
    if (not name or "\0" in name or "\\" in name or name.startswith("/") or
            any(part in ("", ".", "..") for part in name.rstrip("/").split("/"))):
        raise PreflightError("unsafe_archive_path")
    return PurePosixPath(name.rstrip("/"))


def validate_archive(path):
    paths, links, size = {}, set(), 0
    with zipfile.ZipFile(path) as archive:
        members = archive.infolist()
        if not members or len(members) > MAX_MEMBERS:
            raise PreflightError("archive_member_budget_exceeded")
        for member in members:
            name = safe_name(member.orig_filename)
            canonical = unicodedata.normalize("NFD", str(name)).casefold()
            if canonical in paths:
                raise PreflightError("archive_path_alias")
            paths[canonical] = name
            size += member.file_size
            if size > MAX_EXPANDED or member.flag_bits & 1:
                raise PreflightError("archive_size_or_encryption_rejected")
            mode = member.external_attr >> 16
            kind = stat.S_IFMT(mode)
            if kind not in (0, stat.S_IFREG, stat.S_IFDIR, stat.S_IFLNK):
                raise PreflightError("archive_special_file_rejected")
            if kind == stat.S_IFLNK:
                if member.file_size > 4096:
                    raise PreflightError("archive_symlink_rejected")
                target = archive.read(member).decode("utf-8")
                if not target or "\0" in target or "\\" in target or target.startswith("/"):
                    raise PreflightError("archive_symlink_rejected")
                parts = list(name.parent.parts)
                for part in target.split("/"):
                    if part == "..":
                        if not parts:
                            raise PreflightError("archive_symlink_escape")
                        parts.pop()
                    elif part not in ("", "."):
                        parts.append(part)
                links.add(canonical)
        for name in paths.values():
            if any(unicodedata.normalize("NFD", str(parent)).casefold() in links for parent in name.parents):
                raise PreflightError("archive_write_beneath_symlink")
    return {"members": len(members), "expanded_bytes": size}


def inside(path, root):
    try:
        # root is the canonical path captured when this job created its work
        # tree. Never re-resolve it through a replacement symlink at cleanup.
        path.resolve().relative_to(root)
        return True
    except (ValueError, OSError, RuntimeError):
        return False


def verify_extraction(root):
    for path in root.rglob("*"):
        if path.is_symlink():
            if not inside(path, root) or not path.exists():
                raise PreflightError("extracted_symlink_escape_or_missing")
        elif not (path.is_file() or path.is_dir()):
            raise PreflightError("extracted_special_file")


def bundle_metadata(bundle, pins):
    with (bundle / "Contents/Info.plist").open("rb") as stream:
        info = plistlib.load(stream)
    executable = info.get("CFBundleExecutable", "")
    if (not isinstance(executable, str) or not executable or executable in (".", "..") or
            "/" in executable or "\\" in executable or "\0" in executable):
        raise PreflightError("invalid_bundle_executable")
    binary = bundle / "Contents/MacOS" / executable
    if (info.get("CFBundleIdentifier") != pins["bundle_id"] or
            info.get("CFBundleShortVersionString") != pins["version"] or not binary.is_file() or
            not inside(binary, bundle)):
        raise PreflightError("bundle_identity_mismatch")
    build = info.get("CFBundleVersion")
    if not isinstance(build, str) or not re.fullmatch(r"[A-Za-z0-9.()-]{1,80}", build):
        raise PreflightError("invalid_bundle_build")
    return binary.resolve(), {"version": pins["version"], "build": build, "bundle_id": pins["bundle_id"]}


def signature_metadata(output, publisher, team_id):
    authorities = re.findall(r"^Authority=(.+)$", output, re.MULTILINE)
    teams = re.findall(r"^TeamIdentifier=(.+)$", output, re.MULTILINE)
    expected = re.fullmatch(r"Developer ID Application: " + re.escape(publisher) + r" \(([A-Z0-9]{10})\)",
                            authorities[0] if authorities else "")
    if (not expected or expected[1] != team_id or teams != [team_id] or
            authorities[1:] != ["Developer ID Certification Authority", "Apple Root CA"]):
        raise PreflightError("publisher_signature_mismatch")
    return {"publisher": publisher, "team_id": teams[0], "signature": "verified"}


def signature_summary(output, publisher):
    """Retain public certificate identifiers, never arbitrary command output."""
    authorities = re.findall(r"^Authority=(.+)$", output, re.MULTILINE)
    teams = re.findall(r"^TeamIdentifier=(.+)$", output, re.MULTILINE)
    application = re.fullmatch(r"Developer ID Application: ([A-Za-z0-9 ,.&'()-]{1,120}) \(([A-Z0-9]{10})\)",
                               authorities[0] if authorities else "")
    chain = [name if re.fullmatch(r"[A-Za-z0-9 .()-]{1,100}", name) else "unrecognized"
             for name in authorities[1:5]]
    return {"certificate_publisher": application[1] if application else "unrecognized",
            "publisher_matches_pin": bool(application and application[1] == publisher),
            "certificate_team_id": application[2] if application else "unrecognized",
            "team_id": teams[0] if len(teams) == 1 and re.fullmatch(r"[A-Z0-9]{10}", teams[0]) else "unrecognized",
            "certificate_chain": chain}


class ProcBSDInfo(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint32) for name in
                ("flags", "status", "xstatus", "pid", "ppid", "uid", "gid", "ruid", "rgid", "svuid", "svgid", "reserved")]
    _fields_ += [("comm", ctypes.c_char * 16), ("name", ctypes.c_char * 32)]
    _fields_ += [(name, ctypes.c_uint32) for name in ("nfiles", "pgid", "pjobc", "tdev", "tpgid")]
    _fields_ += [("nice", ctypes.c_int32), ("start_sec", ctypes.c_uint64), ("start_usec", ctypes.c_uint64)]


def process_identity(pid):
    if not isinstance(pid, int) or pid <= 1:
        raise PreflightError("invalid_application_pid")
    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    libproc.proc_pidinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int]
    libproc.proc_pidinfo.restype = ctypes.c_int
    info = ProcBSDInfo()
    ctypes.set_errno(0)
    size = libproc.proc_pidinfo(pid, 3, 0, ctypes.byref(info), ctypes.sizeof(info))
    if size <= 0 and ctypes.get_errno() == errno.ESRCH:
        raise ProcessLookupError(pid)
    if size != ctypes.sizeof(info) or info.pid != pid or info.uid != os.getuid() or info.start_sec <= 0:
        raise PreflightError("native_process_identity_unavailable")
    libproc.proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
    libproc.proc_pidpath.restype = ctypes.c_int
    buffer = ctypes.create_string_buffer(4096)
    ctypes.set_errno(0)
    if libproc.proc_pidpath(pid, buffer, len(buffer)) <= 0:
        if ctypes.get_errno() == errno.ESRCH:
            raise ProcessLookupError(pid)
        raise PreflightError("native_process_path_unavailable")
    return {"pid": pid, "start": [info.start_sec, info.start_usec], "executable": os.fsdecode(buffer.value)}


def owned_processes(bundle):
    output, _ = run("/bin/ps", "-axww", "-o", "pid=,comm=")
    result = []
    for line in output.splitlines():
        fields = line.strip().split(None, 1)
        if len(fields) != 2 or not fields[0].isdigit() or not inside(Path(fields[1]), bundle):
            continue
        try:
            identity = process_identity(int(fields[0]))
        except ProcessLookupError:
            continue
        if not inside(Path(identity["executable"]), bundle):
            raise PreflightError("owned_process_identity_changed")
        result.append(identity)
    return result


def require_no_existing_processes():
    output, _ = run("/bin/ps", "-axww", "-o", "pid=,comm=")
    for line in output.splitlines():
        fields = line.strip().split(None, 1)
        if len(fields) != 2:
            continue
        executable = Path(fields[1])
        if ("Claude.app" in executable.parts or executable.name == "Claude" or
                executable.name.startswith("Claude Helper")):
            raise PreflightError("preexisting_desktop_process")


def guarded_signal(identity, bundle, requested_signal):
    if not inside(Path(identity["executable"]), bundle):
        raise PreflightError("cleanup_process_outside_owned_bundle")
    try:
        current = process_identity(identity["pid"])
    except ProcessLookupError:
        return False
    if current != identity:
        return False  # The observed process exited; never signal a reused PID.
    os.kill(identity["pid"], requested_signal)
    return True


def cleanup(bundle, known=None):
    def remaining():
        processes = owned_processes(bundle)
        if known is not None:
            try:
                current = process_identity(known["pid"])
            except ProcessLookupError:
                current = None
            if current == known and current not in processes:
                processes.append(current)
        return processes

    signaled = 0
    for requested_signal, budget in ((signal.SIGTERM, 10), (signal.SIGKILL, 5)):
        deadline = time.monotonic() + budget
        processes = remaining()
        for process in processes:
            try:
                signaled += guarded_signal(process, bundle, requested_signal)
            except ProcessLookupError:
                pass
        while remaining():
            if time.monotonic() >= deadline:
                break
            time.sleep(0.2)
        else:
            return {"status": "passed", "signaled_processes": signaled}
    raise PreflightError("owned_application_processes_remain")


def initial_report():
    return {"schema_version": 1, "kind": "macos-desktop-preflight", "status": "failed", "stage": "environment",
            "desktop_tool_calls": "not_run", "acceptance": "not_run", "release_readiness": "not_established",
            "runtime_barriers": [], "cleanup": {"status": "not_needed"}}


def project_ui_control(value):
    """Accept fixed diagnostics and proof flags, never arbitrary native output."""
    def invalid(field):
        error = PreflightError("invalid_native_ui_control_result")
        # Every field name comes from this fixed schema, never native values.
        error.validation_field = field
        raise error

    def fields(section, mapping, booleans=(), enums=None):
        if not isinstance(mapping, dict):
            invalid(section)
        result = {}
        for name in booleans:
            if type(mapping.get(name)) is not bool:
                invalid(section + "_" + name)
            result[name] = mapping[name]
        for name, choices in (enums or {}).items():
            if not isinstance(mapping.get(name), str) or mapping[name] not in choices:
                invalid(section + "_" + name)
            result[name] = mapping[name]
        return result

    if not isinstance(value, dict) or type(value.get("schema_version")) is not int or value["schema_version"] != 1:
        invalid("schema_version")
    result = {"schema_version": 1, **fields("report", value, ("trusted", "identity_verified"),
              {"status": UI_STATUSES, "controller": {"native-ax"}, "authentication": {"unknown", "sign_in_required"}})}
    result["manual_accessibility"] = fields("manual_accessibility", value.get("manual_accessibility"),
        ("supported", "original_known", "enabled", "restored"), {"enable_result": AX_RESULTS, "restore_result": AX_RESULTS})
    inspection = value.get("inspection")
    result["inspection"] = fields("inspection", inspection,
        ("web_area_seen", "truncated", "sign_in_label", "onboarding_label", "chat_label"))
    if type(inspection.get("elements")) is not int or not 0 <= inspection["elements"] <= 512:
        invalid("inspection_elements")
    counts = inspection.get("role_counts")
    if not isinstance(counts, dict) or any(type(counts.get(role)) is not int or
            not 0 <= counts[role] <= inspection["elements"] for role in UI_ROLES):
        invalid("inspection_role_counts")
    result["inspection"].update(elements=inspection["elements"], role_counts={role: counts[role] for role in UI_ROLES})
    for name in ("owner_lookup_failures", "foreign_owner_elements", "owned_bundle_foreign_elements"):
        if type(inspection.get(name)) is not int or not 0 <= inspection[name] <= 512:
            invalid("inspection_" + name)
        result["inspection"][name] = inspection[name]
    if inspection["owned_bundle_foreign_elements"] > inspection["foreign_owner_elements"]:
        invalid("inspection_ownership_counts")
    for section, readback in (("native_window_control", "minimize_readback"), ("renderer_control", "focus_readback")):
        enums = {"status": UI_STATUSES, "reason": UI_REASONS}
        if section == "renderer_control":
            for phase in ("focus", "restore"):
                enums[phase + "_write_result"] = AX_RESULTS - {"already_enabled", "unchanged", "original_unknown"}
        result[section] = fields(section, value.get(section), ("attempted", readback, "restored"),
                                 enums)
        control = result[section]
        if section == "renderer_control":
            for phase, proof in (("focus", "focus_readback"), ("restore", "restored")):
                count, write_result = phase + "_attempts", phase + "_write_result"
                attempts = value[section].get(count)
                if type(attempts) is not int or not 0 <= attempts <= 32:
                    invalid(section + "_" + count)
                control[count] = attempts
                if ((attempts == 0 and (control[write_result] != "not_attempted" or control[proof])) or
                        (attempts > 0 and (not control["attempted"] or control[write_result] == "not_attempted")) or
                        (control[proof] and control[write_result] != "success")):
                    invalid(section + "_" + phase + "_proof")
        if control["status"] == "passed" and (control["reason"] != "verified" or
                not all(control[key] for key in ("attempted", readback, "restored"))):
            invalid(section + "_proof")
    elapsed = value.get("elapsed_seconds")
    if type(elapsed) not in (int, float) or not math.isfinite(elapsed) or not 0 <= elapsed <= 60:
        invalid("elapsed_seconds")
    barriers = value.get("barriers")
    if not isinstance(barriers, list) or len(barriers) > 20 or any(not isinstance(item, str) or item not in UI_BARRIERS for item in barriers):
        invalid("barriers")
    result.update(elapsed_seconds=round(elapsed, 3), barriers=sorted(set(barriers)))
    if result["authentication"] == "sign_in_required" and not result["inspection"]["sign_in_label"]:
        invalid("authentication_proof")
    if result["renderer_control"]["status"] == "passed" and not result["inspection"]["web_area_seen"]:
        invalid("renderer_exposure_proof")
    if result["status"] == "passed" and (not result["trusted"] or not result["identity_verified"] or
            any(result[section]["status"] != "passed" for section in ("native_window_control", "renderer_control"))):
        invalid("combined_control_proof")
    manual = result["manual_accessibility"]
    if result["status"] == "passed" and manual["enabled"] and (not manual["original_known"] or not manual["restored"]):
        invalid("manual_restoration_proof")
    return result


def probe_ui_control(driver, identity, bundle, pins):
    runtime = Path(os.environ["RUNNER_TEMP"]).resolve(strict=True)
    driver = driver.resolve(strict=True)
    if not driver.is_relative_to(runtime) or not driver.is_file() or not os.access(driver, os.X_OK):
        raise PreflightError("native_ui_driver_not_job_owned")
    driver_digest = digest(driver)
    if process_identity(identity["pid"]) != identity:
        raise PreflightError("application_identity_changed_during_probe")
    command = [str(driver), "--pid", str(identity["pid"]), "--start-sec", str(identity["start"][0]),
               "--start-usec", str(identity["start"][1]), "--executable", identity["executable"],
               "--bundle", str(bundle), "--bundle-id", pins["bundle_id"]]
    try:
        native = subprocess.run(command, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=65)
    except (OSError, subprocess.TimeoutExpired) as exc:
        raise PreflightError("native_ui_driver_unavailable_or_timed_out") from exc
    if process_identity(identity["pid"]) != identity or digest(driver) != driver_digest:
        raise PreflightError("native_ui_probe_identity_changed")
    if len(native.stdout) > 16384:
        raise PreflightError("invalid_native_ui_control_result")
    try:
        result = project_ui_control(json.loads(native.stdout))
    except (ValueError, TypeError) as exc:
        raise PreflightError("invalid_native_ui_control_result") from exc
    if native.returncode not in (0, 1) or (native.returncode == 0) == (result["status"] == "failed"):
        raise PreflightError("native_ui_driver_exit_mismatch")
    result["driver"] = {"binary_sha256": driver_digest,
                        "source_sha256": digest(Path(__file__).resolve().with_name("native_ui.m"))}
    return result


def preflight(pins_path, report, ui_driver=None):
    if os.environ.get("CI") != "true" or platform.system() != "Darwin" or platform.machine() != "arm64":
        raise PreflightError("requires_disposable_native_macos_arm64_ci")
    translated, _ = run("/usr/sbin/sysctl", "-in", "sysctl.proc_translated")
    if translated.strip() != "0":
        raise PreflightError("translated_execution_rejected")
    report["stage"] = "pins"
    pins = load_pins(pins_path)
    version, _ = run("/usr/bin/sw_vers", "-productVersion")
    build, _ = run("/usr/bin/sw_vers", "-buildVersion")
    image_version = os.environ.get("ImageVersion", "unknown")
    if not re.fullmatch(r"[A-Za-z0-9._-]{1,100}", image_version):
        image_version = "unknown"
    report["runner"] = {"os_version": version.strip(), "os_build": build.strip(), "architecture": "arm64",
                        "image_version": image_version}
    report["stage"] = "gui_domain"
    run("/bin/launchctl", "print", "gui/" + str(os.getuid()))
    report["gui_domain"] = "available"
    report["stage"] = "fresh_profile"
    inventory = jxa(INVENTORY, pins["bundle_id"])
    home = Path(inventory["home"]).resolve(strict=True)
    if home != Path(pwd.getpwuid(os.getuid()).pw_dir).resolve(strict=True):
        raise PreflightError("actual_account_home_mismatch")
    if inventory.get("running") != 0:
        raise PreflightError("preexisting_desktop_application")
    require_no_existing_processes()
    profile = home / "Library/Application Support/Claude"
    if os.path.lexists(profile) or os.path.lexists(home / "Library/Containers" / pins["bundle_id"]):
        raise PreflightError("preexisting_desktop_profile")
    report["profile"] = "fresh_actual_account"
    runtime = Path(os.environ.get("RUNNER_TEMP", ""))
    if not runtime.is_absolute() or not runtime.is_dir():
        raise PreflightError("runner_temp_unavailable")
    work = Path(tempfile.mkdtemp(prefix="desktop-preflight-", dir=runtime)).resolve()
    bundle, cleanup_needed, identity = work / "extracted/Claude.app", False, None
    try:
        archive = work / "claude.zip"
        report["stage"] = "download"
        downloaded = download(pins, archive)
        report["stage"] = "archive_safety"
        report["archive"] = {"sha256": pins["sha256"], "download_bytes": downloaded,
                             **validate_archive(archive), "cask_source_revision": pins["cask_source_revision"]}
        extracted = work / "extracted"
        extracted.mkdir()
        report["stage"] = "extraction"
        run("/usr/bin/ditto", "-x", "-k", archive, extracted, timeout=120)
        verify_extraction(extracted)
        if not bundle.is_dir() or bundle.is_symlink():
            raise PreflightError("expected_application_bundle_missing")
        report["stage"] = "bundle_identity"
        binary, metadata = bundle_metadata(bundle, pins)
        frameworks = bundle / "Contents/Frameworks"
        report["ui_frameworks"] = {
            "electron": (frameworks / "Electron Framework.framework").is_dir(),
            "chromium_embedded": (frameworks / "Chromium Embedded Framework.framework").is_dir(),
        }
        architectures, _ = run("/usr/bin/lipo", "-archs", binary)
        if "arm64" not in architectures.split():
            raise PreflightError("native_application_architecture_missing")
        report["stage"] = "signature"
        run("/usr/bin/codesign", "--verify", "--deep", "--strict", bundle, timeout=120)
        stdout, stderr = run("/usr/bin/codesign", "-d", "--verbose=4", bundle)
        report["signature"] = signature_summary(stdout + "\n" + stderr, pins["publisher"])
        metadata.update(signature_metadata(stdout + "\n" + stderr, pins["publisher"], pins["team_id"]))
        report["stage"] = "notarization"
        stdout, stderr = run("/usr/sbin/spctl", "--assess", "--type", "execute", "--verbose=4", bundle, timeout=120)
        if "source=Notarized Developer ID" not in (stdout + "\n" + stderr).splitlines():
            raise PreflightError("notarization_not_confirmed")
        metadata.update(binary_sha256=digest(binary), architectures=architectures.split(), notarization="accepted")
        report["application"] = metadata
        if jxa(INVENTORY, pins["bundle_id"]).get("running") != 0:
            raise PreflightError("preexisting_desktop_application")
        require_no_existing_processes()
        report["stage"] = "launch"
        cleanup_needed = True  # Launch may succeed even if its result cannot be decoded.
        launch_since_us = time.time_ns() // 1000
        launched = jxa(LAUNCH, str(bundle), RENDERER_ACCESSIBILITY_ARGUMENT)
        identity = process_identity(launched.get("pid"))
        if (launched.get("bundle_id") != pins["bundle_id"] or
                Path(launched.get("bundle_path", "")).resolve() != bundle.resolve() or
                Path(launched.get("executable_path", "")).resolve() != binary or
                Path(identity["executable"]).resolve() != binary or
                identity["start"][0] * 1000000 + identity["start"][1] < launch_since_us):
            raise PreflightError("launched_application_identity_mismatch")
        report["process"] = {"pid": identity["pid"], "start": identity["start"],
                             "bundle_path_matches": True, "executable_path_matches": True,
                             "native_identity": "libproc_pid_start_owner_path",
                             "launch_arguments": [RENDERER_ACCESSIBILITY_ARGUMENT]}
        report["stage"] = "ui_control"
        if ui_driver is None:
            raise PreflightError("native_ui_driver_required")
        report["ui_control"] = probe_ui_control(ui_driver, identity, bundle, pins)
        if report["ui_control"]["status"] != "passed":
            raise PreflightError("native_desktop_ui_control_not_established")
        report["ui_control_established"] = True
        if process_identity(identity["pid"]) != identity:
            raise PreflightError("application_identity_changed_during_probe")
        report["stage"] = "post_launch_integrity"
        final_binary, final_metadata = bundle_metadata(bundle, pins)
        if (final_binary != binary or final_metadata != {key: metadata[key] for key in ("version", "build", "bundle_id")} or
                digest(final_binary) != metadata["binary_sha256"]):
            raise PreflightError("application_build_changed_during_probe")
        run("/usr/bin/codesign", "--verify", "--deep", "--strict", bundle, timeout=120)
        if process_identity(identity["pid"]) != identity or digest(binary) != metadata["binary_sha256"]:
            raise PreflightError("application_identity_changed_during_probe")
        report["application"]["post_launch_build_unchanged"] = True
        report["process"]["post_probe_identity_unchanged"] = True
        report["ui_warnings"] = report["ui_control"]["barriers"]
        report["runtime_barriers"].append("desktop_authentication_and_tool_acceptance_not_exercised")
        report["status"] = "completed"
        report["stage"] = "complete"
    finally:
        if cleanup_needed:
            try:
                report["cleanup"] = cleanup(bundle, identity)
            except Exception:
                report["cleanup"] = {"status": "failed", "code": "owned_application_cleanup_failed"}
                report["stage"] = "cleanup"
                raise PreflightError("owned_application_cleanup_failed") from None
        shutil.rmtree(work)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--pins", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--ui-driver", type=Path, required=True)
    arguments = parser.parse_args()
    # Never overwrite an older result or follow an output-directory symlink.
    arguments.out.mkdir(parents=True, exist_ok=False)
    report, status = initial_report(), 1
    try:
        preflight(arguments.pins, report, arguments.ui_driver)
        status = 0
    except ProcessLookupError:
        report["status"] = "failed"
        report["error"] = "owned_application_exited"
    except Exception as exc:
        report["status"] = "failed"
        report["error"] = str(exc) if isinstance(exc, PreflightError) else "unexpected_preflight_failure"
        if isinstance(exc, PreflightError) and hasattr(exc, "validation_field"):
            report["validation_failure"] = exc.validation_field
    finally:
        (arguments.out / "preflight.json").write_text(json.dumps(report, indent=2) + "\n")
    return status


if __name__ == "__main__":
    raise SystemExit(main())
