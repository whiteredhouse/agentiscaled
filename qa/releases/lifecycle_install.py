"""Actual pinned Homebrew install/upgrade around a real enrolled CI service."""
from contextlib import contextmanager
import os
from pathlib import Path
import shutil

from lifecycle_platform import verified_image
from verify import PINS


class HomebrewInstallation:
    def __init__(self, payloads, system, arch, report, run):
        self.payloads, self.system, self.arch = payloads, system, arch
        self.report, self.run = report, run
        # Package-manager state belongs to the real CI user. Only the node's
        # settings, data and generated service use the separate lifecycle HOME.
        self.env = os.environ.copy()
        self.env.update(HOMEBREW_NO_AUTO_UPDATE="1", HOMEBREW_NO_ANALYTICS="1", HOMEBREW_NO_ENV_HINTS="1")
        self.brew = shutil.which("brew") or next((path for path in (
            "/home/linuxbrew/.linuxbrew/bin/brew", "/opt/homebrew/bin/brew", "/usr/local/bin/brew")
            if Path(path).is_file()), None)
        self.attempted = False
        self.stable = None

    def command(self, *arguments, **kwargs):
        return self.run(*arguments, env=self.env, **kwargs)

    def checked_binary(self, item):
        release, _, expected_digest = item
        image = verified_image(self.stable, expected_digest)
        expected = f'agentiscaled {release["version"]} (commit {release["commit"][:12]}, {self.system}/{self.arch})'
        if self.command(self.stable, "version") != expected:
            raise ValueError("Homebrew installed executable version differs from reviewed release")
        if "/Caskroom/agentiscaled/" + release["version"] + "/" not in image["path"]:
            raise ValueError("Homebrew stable binary does not select the exact reviewed cask version")
        return image

    def install(self):
        if not self.brew:
            raise RuntimeError("lifecycle requires the pinned Homebrew installation prepared by native consumer CI")
        repository = Path(self.brew).resolve().parent.parent
        commit = self.command("git", "-C", repository, "rev-parse", "HEAD")
        if commit != PINS["environment"]["homebrew"]["commit"]:
            raise ValueError("lifecycle Homebrew checkout differs from reviewed baseline")
        self.tap = self.command(self.brew, "--repo", "whiteredhouse/tap").splitlines()[-1]
        prefix = Path(self.command(self.brew, "--prefix").splitlines()[-1])
        if not prefix.is_absolute():
            raise ValueError("Homebrew prefix must be absolute")
        # Preserve this alias: stableExecutable in the actual release must
        # select brew/bin rather than the versioned Caskroom target.
        self.stable = prefix / "bin/agentiscaled"
        if self.stable.exists() or self.stable.is_symlink():
            raise ValueError("lifecycle must not overwrite an existing Homebrew executable")
        self.report["upgrade_method"] = {"manager": "Homebrew", "stable_binary": str(self.stable),
                                         "brew_commit": commit, "operations": []}
        release = self.payloads[0][0]
        self.command("git", "-C", self.tap, "checkout", "--detach", release["tap_commit"])
        self.attempted = True
        self.command(self.brew, "install", "--cask", "whiteredhouse/tap/agentiscaled", timeout=1200)
        self.old_image = self.checked_binary(self.payloads[0])
        self.report["upgrade_method"]["operations"].append({"operation": "install", "version": release["version"]})

    def upgrade(self):
        release = self.payloads[1][0]
        self.command("git", "-C", self.tap, "checkout", "--detach", release["tap_commit"])
        self.command(self.brew, "upgrade", "--cask", "whiteredhouse/tap/agentiscaled", timeout=1200)
        image = self.checked_binary(self.payloads[1])
        if image["sha256"] == self.old_image["sha256"]:
            raise ValueError("Homebrew upgrade did not change verified release bytes")
        if Path(self.old_image["path"]).exists():
            raise ValueError("Homebrew upgrade did not remove the old versioned executable")
        self.report["upgrade_method"].update(old_versioned_executable_removed=True)
        self.report["upgrade_method"]["operations"].append({"operation": "upgrade", "version": release["version"]})
        return image

    def cleanup(self):
        if self.attempted:
            try:
                self.command(self.brew, "uninstall", "--cask", "whiteredhouse/tap/agentiscaled", timeout=1200)
                if self.stable.exists() or self.stable.is_symlink():
                    raise RuntimeError("Homebrew stable executable remains after uninstall")
                self.report["homebrew_cleanup"] = "passed"
            except Exception as exc:
                self.report["homebrew_cleanup"] = str(exc)
                self.report["cleanup"] = ["Homebrew cleanup failed: " + str(exc)]
                raise


@contextmanager
def homebrew_installation(payloads, system, arch, report, run):
    installation = HomebrewInstallation(payloads, system, arch, report, run)
    try:
        installation.install()
        yield installation
    finally:
        # The caller exits exercise (and its supervisor cleanup) before this.
        installation.cleanup()
