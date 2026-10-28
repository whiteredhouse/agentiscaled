"""Installed-image controls for actual Homebrew lifecycle verification."""
import hashlib
from pathlib import Path
import tempfile
import unittest

from lifecycle_install import HomebrewInstallation


class HomebrewImageTests(unittest.TestCase):
    def test_installed_image_requires_release_hash_version_and_cask_target(self):
        release = {"version": "0.1.10", "commit": "c41a62a120be89fef6417fc67779e759cf672ec2"}
        expected_version = "agentiscaled 0.1.10 (commit c41a62a120be, linux/amd64)"
        with tempfile.TemporaryDirectory() as directory:
            prefix = Path(directory)
            target = prefix / "Caskroom/agentiscaled/0.1.10/agentiscaled"
            target.parent.mkdir(parents=True)
            target.write_bytes(b"published-release")
            item = (release, prefix, hashlib.sha256(b"published-release").hexdigest())
            installation = HomebrewInstallation([item, item], "linux", "amd64", {}, lambda *args, **kwargs: expected_version)
            installation.stable = prefix / "bin/agentiscaled"
            installation.stable.parent.mkdir()
            installation.stable.symlink_to(target)
            self.assertEqual(installation.checked_binary(item)["path"], str(target))
            target.write_bytes(b"mutated-release")
            with self.assertRaisesRegex(ValueError, "verified release"):
                installation.checked_binary(item)
            target.write_bytes(b"published-release")
            installation.run = lambda *args, **kwargs: "agentiscaled 0.1.11 (commit wrong, linux/amd64)"
            with self.assertRaisesRegex(ValueError, "version"):
                installation.checked_binary(item)
            installation.run = lambda *args, **kwargs: expected_version
            wrong = prefix / "Caskroom/agentiscaled/0.1.11/agentiscaled"
            wrong.parent.mkdir(parents=True)
            wrong.write_bytes(b"published-release")
            installation.stable.unlink()
            installation.stable.symlink_to(wrong)
            with self.assertRaisesRegex(ValueError, "exact reviewed cask version"):
                installation.checked_binary(item)


if __name__ == "__main__":
    unittest.main()
