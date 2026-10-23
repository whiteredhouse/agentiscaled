"""Controls for fixture ownership across sequential native release pairs."""
from pathlib import Path
import shutil
import tempfile
import unittest
from unittest.mock import patch

from native import cask_operations, sentinel_files


class NativeCleanup(unittest.TestCase):
    def test_fixture_files_are_removed_after_failure_and_can_be_reused(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "config" / "agentiscaled.json"
            with self.assertRaisesRegex(RuntimeError, "upgrade failed"):
                with sentinel_files([path]):
                    path.write_bytes(b"changed during failed upgrade")
                    raise RuntimeError("upgrade failed")
            self.assertFalse(path.exists())
            with sentinel_files([path]):
                self.assertTrue(path.is_file())
            self.assertFalse(path.exists())

    def test_existing_state_is_never_removed_when_fixture_creation_fails(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            owned, existing = root / "owned", root / "existing"
            existing.write_bytes(b"existing state")
            with self.assertRaisesRegex(ValueError, "existing state"):
                with sentinel_files([owned, existing]):
                    self.fail("existing state must prevent entry")
            self.assertFalse(owned.exists())
            self.assertEqual(existing.read_bytes(), b"existing state")

    def test_cleanup_refuses_a_replacement_file(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path, replacement = root / "sentinel", root / "replacement"
            replacement.write_bytes(b"unowned replacement")
            with self.assertRaisesRegex(RuntimeError, "ownership changed"):
                with sentinel_files([path]):
                    replacement.replace(path)
            self.assertEqual(path.read_bytes(), b"unowned replacement")

    def test_partial_cask_install_is_removed_after_command_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            prefix = Path(temporary)
            cask = prefix / "Caskroom" / "agentiscaled"
            stable = prefix / "bin" / "agentiscaled"

            def brew(*command, **kwargs):
                operation = command[1]
                if operation == "--prefix":
                    return str(prefix)
                if operation == "--caskroom":
                    return str(prefix / "Caskroom")
                if operation == "install":
                    cask.mkdir(parents=True)
                    stable.parent.mkdir()
                    stable.symlink_to(cask / "partial-install")
                    raise RuntimeError("install interrupted")
                if operation == "uninstall":
                    shutil.rmtree(cask)
                    stable.unlink()
                    return ""
                self.fail(f"unexpected brew command: {command}")

            with patch("native.run", side_effect=brew):
                with self.assertRaisesRegex(RuntimeError, "install interrupted"):
                    with cask_operations("brew", {}) as operate:
                        operate("install")
            self.assertFalse(cask.exists())
            self.assertFalse(stable.is_symlink())

    def test_preexisting_cask_is_not_owned_by_the_fixture(self):
        with tempfile.TemporaryDirectory() as temporary:
            prefix = Path(temporary)
            cask = prefix / "Caskroom" / "agentiscaled"
            cask.mkdir(parents=True)
            marker = cask / "existing"
            marker.write_bytes(b"existing cask")
            with patch("native.run", side_effect=[str(prefix), str(prefix / "Caskroom")]):
                with self.assertRaisesRegex(ValueError, "existing Homebrew"):
                    with cask_operations("brew", {}):
                        self.fail("preexisting cask must prevent entry")
            self.assertEqual(marker.read_bytes(), b"existing cask")
