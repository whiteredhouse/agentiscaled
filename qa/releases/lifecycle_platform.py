"""Native supervisor and loaded-image evidence for disposable CI runners.

These helpers never install an alternative service. The release's own `up`
command must generate and start the systemd unit or launchd LaunchAgent.
"""
import ctypes
import hashlib
import os
from pathlib import Path
import plistlib
import re
import time


def digest(path):
    with Path(path).open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def native_target(system, machine, apple_arm64=False):
    targets = {("Linux", "x86_64"): ("linux", "amd64"),
               ("Linux", "aarch64"): ("linux", "arm64"),
               ("Darwin", "x86_64"): ("darwin", "amd64"),
               ("Darwin", "arm64"): ("darwin", "arm64")}
    if (system, machine) not in targets:
        raise ValueError("lifecycle requires native Linux or macOS amd64/arm64")
    if system == "Darwin" and machine == "x86_64" and apple_arm64:
        raise ValueError("translated macOS execution cannot prove a native amd64 lifecycle")
    return targets[(system, machine)]


def parse_launchctl(output):
    # launchctl print contains nested dictionaries with their own state/PIDs.
    # Only direct fields in the service dictionary describe the service.
    depth, values, saw_root = 0, {}, False
    for line in output.splitlines():
        stripped = line.strip()
        if depth == 1:
            match = re.fullmatch(r"(state|pid|program|path) = (.+)", stripped)
            if match:
                key, value = match.groups()
                if key in values:
                    raise ValueError("duplicate launchd service property")
                values[key] = value
        depth += stripped.count("{") - stripped.count("}")
        if depth < 0:
            raise ValueError("malformed launchctl service dictionary")
        saw_root = saw_root or depth > 0
    if not saw_root or depth != 0 or values.get("state") != "running" or not re.fullmatch(r"[0-9]+", values.get("pid", "")):
        raise ValueError("launchd did not report a running service PID")
    values["pid"] = int(values["pid"])
    if values["pid"] <= 1:
        raise ValueError("launchd reported an invalid service PID")
    return values


def parse_linux_start(stat):
    # comm is parenthesized and may itself contain spaces and parentheses.
    fields = stat.rsplit(") ", 1)[-1].split()
    if len(fields) < 20 or not fields[19].isdigit() or int(fields[19]) <= 0:
        raise ValueError("invalid /proc PID start-time evidence")
    return int(fields[19])


def parse_lsof(output, pid):
    rows, current, process = [], None, None
    for line in output.splitlines():
        if not line:
            continue
        kind, value = line[0], line[1:]
        if kind == "p":
            if not value.isdigit() or int(value) != pid:
                raise ValueError("lsof output belongs to another process")
            process = int(value)
        elif kind == "f":
            if process != pid:
                raise ValueError("lsof file lacks owning PID")
            current = {"descriptor": value}
            rows.append(current)
        elif kind in ("D", "i", "n") and current is not None:
            key = {"D": "device", "i": "inode", "n": "path"}[kind]
            if key in current:
                raise ValueError("duplicate lsof mapped-file property")
            try:
                current[key] = value if kind == "n" else int(value, 16 if kind == "D" else 10)
            except ValueError as exc:
                raise ValueError("invalid lsof mapped-file identity") from exc
    if process != pid:
        raise ValueError("lsof did not return the supervisor PID")
    return rows


def validate_mapping(rows, expected):
    matches = [row for row in rows if row.get("descriptor") == "txt" and
               all(row.get(key) == expected[key] for key in ("path", "device", "inode"))]
    if len(matches) != 1:
        raise ValueError("actual mapped executable path/device/inode differs from verified release file")
    return matches[0]


def validate_fresh_process(before, after, previous=None, since_us=None):
    if before["pid"] <= 1 or (before["pid"], before["start"]) != (after["pid"], after["start"]):
        raise ValueError("service PID/start changed while collecting loaded-image evidence")
    if previous is not None and before["pid"] == previous["pid"]:
        raise ValueError("service restart did not produce a fresh PID")
    if since_us is not None:
        seconds, micros = before["start"]
        if seconds * 1_000_000 + micros < since_us:
            raise ValueError("service process predates the requested fresh start")


def verified_image(path, expected_digest):
    path = Path(path).resolve(strict=True)
    with path.open("rb") as stream:
        before = os.fstat(stream.fileno())
        actual = hashlib.file_digest(stream, "sha256").hexdigest()
        after = os.fstat(stream.fileno())
    fields = ("st_dev", "st_ino", "st_size", "st_mtime_ns", "st_ctime_ns")
    if any(getattr(before, field) != getattr(after, field) for field in fields) or actual != expected_digest:
        raise ValueError("stable executable changed or differs from verified release bytes")
    return {"path": str(path), "device": before.st_dev, "inode": before.st_ino,
            "size": before.st_size, "mtime_ns": before.st_mtime_ns,
            "ctime_ns": before.st_ctime_ns, "sha256": actual}


class LinuxSupervisor:
    kind = "systemd user service"

    def __init__(self, env, home, data, stable, run):
        self.env, self.data, self.stable, self.run = env, data, stable, run
        suffix = hashlib.sha256(str(data).encode()).hexdigest()[:8]
        self.name = "agentiscaled-" + suffix + ".service"
        self.path = home / ".config/systemd/user" / self.name
        self.linked = False

    def ctl(self, *args, **kwargs):
        return self.run("systemctl", "--user", *args, env=self.env, **kwargs)

    def prepare(self):
        if not self.env.get("XDG_RUNTIME_DIR") or not Path(self.env["XDG_RUNTIME_DIR"]).is_dir():
            raise RuntimeError("real systemd user runtime is unavailable; CI must enable linger and the user bus")
        self.ctl("show", "--property=Version", "--value")
        self.path.parent.mkdir(parents=True)
        # Only a discovery link for the existing user manager; `up` overwrites
        # the bootstrap with its actual generated unit before it can run.
        self.path.write_text("[Service]\nExecStart=/usr/bin/false\n[Install]\nWantedBy=default.target\n")
        self.ctl("link", "--runtime", self.path)
        self.linked = True

    def validate_generated(self):
        values = self.properties()
        if Path(values.get("FragmentPath", "")).resolve() != self.path.resolve():
            raise ValueError("systemd loaded a different unit file")
        contents = self.path.read_text()
        if "/usr/bin/false" in contents or "ExecStart=" not in contents:
            raise ValueError("release up did not generate its real service unit")

    def properties(self):
        values = {}
        for line in self.ctl("show", self.name, "--property=MainPID,ActiveState,SubState,FragmentPath").splitlines():
            key, value = line.split("=", 1)
            if key in values:
                raise ValueError("duplicate systemd property")
            values[key] = value
        if values.get("ActiveState") != "active" or values.get("SubState") != "running" or int(values.get("MainPID", "0")) <= 1:
            raise ValueError("systemd did not report an active running MainPID")
        return values

    def identity(self):
        values = self.properties()
        pid = int(values["MainPID"])
        start = parse_linux_start((Path("/proc") / str(pid) / "stat").read_text())
        return {"pid": pid, "start": start, "supervisor": values}

    def evidence(self, image, since_us, previous, ready):
        before = self.identity()
        process = Path("/proc") / str(before["pid"])
        actual_digest = digest(process / "exe")
        if actual_digest != image["sha256"]:
            raise ValueError("actual /proc/MainPID/exe bytes differ from verified release executable")
        arguments = (process / "cmdline").read_bytes().split(b"\0")
        expected = [str(self.stable).encode(), b"run", b"-data-dir", str(self.data).encode()]
        if arguments[:4] != expected:
            raise ValueError("systemd MainPID is not running the isolated enrolled node")
        mesh = ready()
        validate_fresh_process(before, self.identity(), previous=previous)
        if verified_image(self.stable, image["sha256"]) != image:
            raise ValueError("verified executable identity changed during observation")
        return dict(before, proc_exe=os.readlink(process / "exe"), proc_exe_sha256=actual_digest,
                    proof="SHA-256 of actual /proc/MainPID/exe", mesh=mesh)

    def stop(self):
        self.ctl("stop", self.name)

    def start(self):
        self.ctl("start", self.name)

    def restart(self):
        self.ctl("restart", self.name)

    def cleanup(self):
        errors = []
        if not self.linked:
            return errors
        for arguments in (("stop", self.name), ("disable", self.name), ("disable", "--runtime", self.name)):
            try:
                self.ctl(*arguments)
            except Exception as exc:
                errors.append(str(exc))
        runtime_link = Path(self.env["XDG_RUNTIME_DIR"]) / "systemd/user" / self.name
        if runtime_link.is_symlink() and runtime_link.resolve() == self.path:
            runtime_link.unlink()
        try:
            self.ctl("daemon-reload")
        except Exception as exc:
            errors.append(str(exc))
        return errors


# Public macOS libproc ABI, declared by Apple's bsd/sys/proc_info.h.
# https://github.com/apple-oss-distributions/xnu/blob/main/bsd/sys/proc_info.h
class ProcBSDInfo(ctypes.Structure):
    _fields_ = [(name, ctypes.c_uint32) for name in
                ("flags", "status", "xstatus", "pid", "ppid", "uid", "gid", "ruid", "rgid", "svuid", "svgid", "reserved")]
    _fields_ += [("comm", ctypes.c_char * 16), ("name", ctypes.c_char * 32)]
    _fields_ += [(name, ctypes.c_uint32) for name in ("nfiles", "pgid", "pjobc", "tdev", "tpgid")]
    _fields_ += [("nice", ctypes.c_int32), ("start_sec", ctypes.c_uint64), ("start_usec", ctypes.c_uint64)]


def darwin_process(pid):
    libproc = ctypes.CDLL("/usr/lib/libproc.dylib", use_errno=True)
    libproc.proc_pidinfo.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_uint64, ctypes.c_void_p, ctypes.c_int]
    libproc.proc_pidinfo.restype = ctypes.c_int
    info = ProcBSDInfo()
    # PROC_PIDTBSDINFO = 3; exact returned size fails closed on an ABI mismatch.
    size = libproc.proc_pidinfo(pid, 3, 0, ctypes.byref(info), ctypes.sizeof(info))
    if size != ctypes.sizeof(info) or info.pid != pid or info.uid != os.getuid() or info.start_sec <= 0:
        raise RuntimeError("cannot read exact native libproc PID/start/owner evidence")
    libproc.proc_pidpath.argtypes = [ctypes.c_int, ctypes.c_void_p, ctypes.c_uint32]
    libproc.proc_pidpath.restype = ctypes.c_int
    path = ctypes.create_string_buffer(4096)
    if libproc.proc_pidpath(pid, path, len(path)) <= 0:
        raise RuntimeError("cannot read actual libproc executable path")
    return {"pid": pid, "start": [info.start_sec, info.start_usec], "uid": info.uid,
            "executable_path": os.fsdecode(path.value)}


class DarwinSupervisor:
    kind = "launchd GUI LaunchAgent"

    def __init__(self, env, home, data, stable, run):
        self.env, self.data, self.stable, self.run = env, data, stable, run
        suffix = hashlib.sha256(str(data).encode()).hexdigest()[:8]
        self.name = "net.agentiscale.agentiscaled." + suffix
        self.domain = "gui/" + str(os.getuid())
        self.target = self.domain + "/" + self.name
        self.path = home / "Library/LaunchAgents" / (self.name + ".plist")
        self.prepared = False
        self.last_process = None

    def ctl(self, *args, **kwargs):
        return self.run("/bin/launchctl", *args, env=self.env, **kwargs)

    def prepare(self):
        self.ctl("print", self.domain)
        if not Path("/usr/sbin/lsof").is_file():
            raise RuntimeError("native macOS mapped-image proof requires /usr/sbin/lsof")
        self.prepared = True

    def validate_generated(self):
        with self.path.open("rb") as stream:
            plist = plistlib.load(stream)
        if (plist.get("Label") != self.name or plist.get("ProgramArguments") !=
                [str(self.stable), "run", "-data-dir", str(self.data)] or
                plist.get("RunAtLoad") is not True or plist.get("KeepAlive") is not True):
            raise ValueError("release up did not generate its real isolated launchd service")
        self.identity()

    def identity(self):
        values = parse_launchctl(self.ctl("print", self.target))
        result = darwin_process(values["pid"])
        if Path(result["executable_path"]).resolve() != self.stable.resolve():
            raise ValueError("launchd PID executable path differs from the isolated release executable")
        self.last_process = result
        return dict(result, supervisor=values)

    def evidence(self, image, since_us, previous, ready):
        before = self.identity()
        output = self.run("/usr/sbin/lsof", "-a", "-p", before["pid"], "-d", "txt", "-F", "pfnDi", env=self.env)
        mapping = validate_mapping(parse_lsof(output, before["pid"]), image)
        mesh = ready()
        after = self.identity()
        validate_fresh_process(before, after, previous=previous, since_us=since_us)
        if verified_image(self.stable, image["sha256"]) != image:
            raise ValueError("verified executable identity changed during loaded-image observation")
        # macOS does not expose /proc/PID/exe. This is an inode-bound proof:
        # SHA of the file verified before this fresh start, actual txt mapping
        # to that same device/inode, plus stable kernel start time and PID.
        return dict(before, mapped_executable=mapping, verified_file=image,
                    proc_exe_sha256=image["sha256"],
                    proc_exe_sha256_derivation="compatibility field: verified-file SHA bound to actual mapped device/inode and fresh PID/start",
                    proof="fresh libproc PID/start plus lsof txt device/inode bound to verified SHA-256 file",
                    daemon_version_endpoint=False, mesh=mesh)

    def wait_stopped(self, identity):
        if identity is None:
            return
        deadline = time.monotonic() + 30
        while True:
            try:
                os.kill(identity["pid"], 0)
            except ProcessLookupError:
                return
            # PID reuse is distinguishable from the stopped process.
            current = darwin_process(identity["pid"])
            if current["start"] != identity["start"]:
                return
            if time.monotonic() >= deadline:
                raise RuntimeError("launchd process remains alive after bootout")
            time.sleep(0.1)

    def stop(self):
        identity = self.identity()
        self.ctl("bootout", self.target)
        self.wait_stopped(identity)

    def start(self):
        self.ctl("bootstrap", self.domain, self.path)

    def restart(self):
        self.stop()
        self.start()

    def cleanup(self):
        errors = []
        if not self.prepared or not self.path.exists():
            return errors
        # print distinguishes an already stopped service from a failed bootout.
        try:
            loaded = self.run("/bin/launchctl", "print", self.target, env=self.env, check=False)
            if loaded:
                self.ctl("bootout", self.target)
            self.wait_stopped(self.last_process)
            remaining = self.run("/bin/launchctl", "print", self.target, env=self.env, check=False)
            if remaining:
                raise RuntimeError("launchd agent remains loaded after cleanup")
            self.path.unlink()
        except Exception as exc:
            errors.append(str(exc))
        return errors
