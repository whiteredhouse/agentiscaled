"""Negative controls for the real app probe; no desktop or account is mocked as accepted."""
import hashlib
import io
import json
from pathlib import Path
import signal
import stat
import tempfile
import unittest
from unittest import mock
import zipfile

import preflight


class PreflightControls(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        self.root = Path(directory.name).resolve()

    def archive(self, entries):
        target = self.root / "app.zip"
        with zipfile.ZipFile(target, "w") as archive:
            for name, content, mode in entries:
                member = zipfile.ZipInfo(name)
                member.create_system = 3
                member.external_attr = mode << 16
                archive.writestr(member, content)
        return target

    def test_framework_symlinks_are_allowed_without_extracting(self):
        path = self.archive([
            ("Claude.app/Contents/Frameworks/F.framework/Versions/A/Resources/info", "data", stat.S_IFREG | 0o644),
            ("Claude.app/Contents/Frameworks/F.framework/Versions/Current", "A", stat.S_IFLNK | 0o777),
            ("Claude.app/Contents/Frameworks/F.framework/Resources", "Versions/Current/Resources", stat.S_IFLNK | 0o777)])
        self.assertEqual(preflight.validate_archive(path), {"members": 3, "expanded_bytes": 31})
        self.assertFalse((self.root / "Claude.app").exists())

    def test_archive_rejects_traversal_absolute_paths_and_aliases(self):
        for name in ("../outside", "/absolute", "app/../../outside", "app\\outside", "app//file", "./app"):
            with self.subTest(name=name), self.assertRaises(preflight.PreflightError):
                preflight.validate_archive(self.archive([(name, "x", stat.S_IFREG | 0o644)]))
        for names in (("app/file", "app/FILE"), ("app/\N{LATIN SMALL LETTER E WITH ACUTE}", "app/e\N{COMBINING ACUTE ACCENT}")):
            with self.subTest(names=names), self.assertRaisesRegex(preflight.PreflightError, "path_alias"):
                preflight.validate_archive(self.archive([(name, "x", stat.S_IFREG | 0o644) for name in names]))
        with self.assertRaises(preflight.PreflightError):
            preflight.safe_name("app/secret\0ignored")

    def test_archive_rejects_escaping_links_write_through_and_special_files(self):
        cases = [
            [("app/link", "../../outside", stat.S_IFLNK | 0o777)],
            [("app/link", "/outside", stat.S_IFLNK | 0o777)],
            [("app/link", "target", stat.S_IFLNK | 0o777), ("app/link/file", "x", stat.S_IFREG | 0o644)],
            [("app/pipe", "", stat.S_IFIFO | 0o600)],
        ]
        for entries in cases:
            with self.subTest(entries=entries), self.assertRaises(preflight.PreflightError):
                preflight.validate_archive(self.archive(entries))

    def test_archive_enforces_expanded_size_and_member_budgets(self):
        archive = self.archive([("app/file", "payload", stat.S_IFREG | 0o644)])
        with mock.patch.object(preflight, "MAX_EXPANDED", 3), self.assertRaisesRegex(
                preflight.PreflightError, "size_or_encryption"):
            preflight.validate_archive(archive)
        with mock.patch.object(preflight, "MAX_MEMBERS", 0), self.assertRaisesRegex(
                preflight.PreflightError, "member_budget"):
            preflight.validate_archive(archive)

    def test_extracted_symlinks_cannot_escape_or_remain_broken(self):
        extracted = self.root / "extracted"
        extracted.mkdir()
        link = extracted / "link"
        for target in (self.root, extracted / "missing"):
            link.symlink_to(target)
            with self.subTest(target=target), self.assertRaisesRegex(preflight.PreflightError, "symlink_escape_or_missing"):
                preflight.verify_extraction(extracted)
            link.unlink()

    def test_download_requires_exact_reviewed_bytes_and_budget(self):
        payload = b"reviewed archive"
        pins = {"url": "https://downloads.claude.ai/app.zip", "sha256": hashlib.sha256(payload).hexdigest()}

        class Response(io.BytesIO):
            def geturl(self):
                return pins["url"]

        for digest, budget, passes in ((pins["sha256"], 100, True), ("0" * 64, 100, False), (pins["sha256"], 3, False)):
            destination = self.root / "download.zip"
            with self.subTest(digest=digest, budget=budget), mock.patch(
                    "preflight.urllib.request.urlopen", return_value=Response(payload)), mock.patch.object(preflight, "MAX_DOWNLOAD", budget):
                if passes:
                    self.assertEqual(preflight.download(dict(pins, sha256=digest), destination), len(payload))
                else:
                    with self.assertRaises(preflight.PreflightError):
                        preflight.download(dict(pins, sha256=digest), destination)
            destination.unlink()

    def test_signature_requires_exact_publisher_matching_team_and_apple_chain(self):
        signature = ("Authority=Developer ID Application: Anthropic, PBC (ABCDEFGHIJ)\n"
                     "Authority=Developer ID Certification Authority\nAuthority=Apple Root CA\nTeamIdentifier=ABCDEFGHIJ\n")
        self.assertEqual(preflight.signature_metadata(signature, "Anthropic, PBC", "ABCDEFGHIJ"),
                         {"publisher": "Anthropic, PBC", "team_id": "ABCDEFGHIJ", "signature": "verified"})
        for changed in (signature.replace("Anthropic, PBC", "Not Anthropic, PBC"),
                        signature.replace("TeamIdentifier=ABCDEFGHIJ", "TeamIdentifier=0123456789"),
                        signature.replace("Authority=Apple Root CA\n", ""),
                        signature + "TeamIdentifier=ABCDEFGHIJ\n"):
            with self.subTest(changed=changed), self.assertRaisesRegex(preflight.PreflightError, "publisher_signature"):
                preflight.signature_metadata(changed, "Anthropic, PBC", "ABCDEFGHIJ")
        # A different valid Developer ID with matching certificate/declared
        # team fields still cannot replace this reviewed publisher identity.
        with self.assertRaisesRegex(preflight.PreflightError, "publisher_signature"):
            preflight.signature_metadata(signature.replace("ABCDEFGHIJ", "0123456789"), "Anthropic, PBC", "ABCDEFGHIJ")

    def test_signature_diagnostics_retain_only_public_certificate_identifiers(self):
        signature = ("Authority=Developer ID Application: Another Publisher (ABCDEFGHIJ)\n"
                     "Authority=Developer ID Certification Authority\nAuthority=Apple Root CA - G3\n"
                     "TeamIdentifier=ABCDEFGHIJ\nSensitiveExtra=not retained\n")
        self.assertEqual(preflight.signature_summary(signature, "Anthropic, PBC"), {
            "certificate_publisher": "Another Publisher", "publisher_matches_pin": False,
            "certificate_team_id": "ABCDEFGHIJ", "team_id": "ABCDEFGHIJ",
            "certificate_chain": ["Developer ID Certification Authority", "Apple Root CA - G3"]})
        changed = preflight.signature_summary(signature.replace("Another Publisher", "https://private.invalid/token"), "Anthropic, PBC")
        self.assertEqual(changed["certificate_publisher"], "unrecognized")
        self.assertNotIn("private.invalid", json.dumps(changed))

    def test_bundle_executable_must_be_a_basename_inside_bundle(self):
        import plistlib
        bundle = self.root / "Claude.app"
        (bundle / "Contents/MacOS").mkdir(parents=True)
        (bundle / "Contents/MacOS/Claude").write_bytes(b"binary")
        pins = {"bundle_id": "com.anthropic.claudefordesktop", "version": "4.4212.0"}
        info = {"CFBundleExecutable": "Claude", "CFBundleIdentifier": pins["bundle_id"],
                "CFBundleShortVersionString": pins["version"], "CFBundleVersion": "4212"}
        for executable in ("Claude", "../Info.plist", "/bin/other", "..", "Claude\\other"):
            with (bundle / "Contents/Info.plist").open("wb") as stream:
                plistlib.dump(dict(info, CFBundleExecutable=executable), stream)
            if executable == "Claude":
                self.assertEqual(preflight.bundle_metadata(bundle, pins)[0], bundle / "Contents/MacOS/Claude")
            else:
                with self.subTest(executable=executable), self.assertRaises(preflight.PreflightError):
                    preflight.bundle_metadata(bundle, pins)

    def test_cleanup_never_signals_a_reused_or_outside_process(self):
        bundle = self.root / "Claude.app"
        identity = {"pid": 4123, "start": [1700000000, 200], "executable": str(bundle / "Contents/MacOS/Claude")}
        for current in (dict(identity, start=[1700000000, 201]), dict(identity, executable="/bin/unrelated")):
            with self.subTest(current=current), mock.patch("preflight.process_identity", return_value=current), mock.patch(
                    "preflight.os.kill") as kill:
                self.assertFalse(preflight.guarded_signal(identity, bundle, signal.SIGTERM))
                kill.assert_not_called()
        with mock.patch("preflight.process_identity", side_effect=ProcessLookupError), mock.patch("preflight.os.kill") as kill:
            self.assertFalse(preflight.guarded_signal(identity, bundle, signal.SIGTERM))
            kill.assert_not_called()
        with mock.patch("preflight.os.kill") as kill, self.assertRaisesRegex(preflight.PreflightError, "outside_owned_bundle"):
            preflight.guarded_signal(dict(identity, executable="/bin/unrelated"), bundle, signal.SIGKILL)
        with mock.patch("preflight.process_identity", return_value=identity), mock.patch("preflight.os.kill") as kill:
            self.assertTrue(preflight.guarded_signal(identity, bundle, signal.SIGTERM))
            kill.assert_called_once_with(4123, signal.SIGTERM)

    def test_cleanup_anchor_cannot_be_redirected_to_another_application(self):
        other = self.root / "other/Claude.app"
        (other / "Contents/MacOS").mkdir(parents=True)
        (other / "Contents/MacOS/Claude").write_bytes(b"unrelated")
        owned = self.root / "owned/Claude.app"
        owned.parent.mkdir()
        owned.symlink_to(other)
        identity = {"pid": 4123, "start": [1700000000, 200], "executable": str(other / "Contents/MacOS/Claude")}
        with mock.patch("preflight.os.kill") as kill, self.assertRaisesRegex(preflight.PreflightError, "outside_owned_bundle"):
            preflight.guarded_signal(identity, owned, signal.SIGTERM)
        kill.assert_not_called()

    def test_cleanup_still_checks_known_main_process_when_process_listing_omits_it(self):
        bundle = self.root / "Claude.app"
        identity = {"pid": 4123, "start": [1700000000, 200], "executable": str(bundle / "Contents/MacOS/Claude")}
        with mock.patch("preflight.owned_processes", side_effect=lambda bundle: []), mock.patch(
                "preflight.process_identity", side_effect=[identity, identity, ProcessLookupError]), mock.patch("preflight.os.kill") as kill:
            self.assertEqual(preflight.cleanup(bundle, identity), {"status": "passed", "signaled_processes": 1})
            kill.assert_called_once_with(4123, signal.SIGTERM)


    def test_pin_manifest_rejects_mutable_wrong_origin_or_unknown_fields(self):
        pins = {"version": "4.4212.0", "url": "https://downloads.claude.ai/releases/darwin/universal/4.4212.0/Claude.zip",
                "sha256": "e" * 64, "bundle_id": "com.anthropic.claudefordesktop", "publisher": "Anthropic, PBC",
                "team_id": "ABCDEFGHIJ", "cask_source_revision": "a" * 40}
        path = self.root / "pins.json"
        path.write_text(json.dumps(pins))
        self.assertEqual(preflight.load_pins(path), pins)
        for changed in ({"sha256": "latest"}, {"url": pins["url"].replace("downloads.claude.ai", "elsewhere.invalid")},
                        {"url": pins["url"] + "?token=secret"}, {"version": "latest"}, {"team_id": "unreviewed"},
                        {"unexpected": "field"}):
            path.write_text(json.dumps(dict(pins, **changed)))
            with self.subTest(changed=changed), self.assertRaises(preflight.PreflightError):
                preflight.load_pins(path)

    def test_execution_refuses_non_macos_and_non_ci_hosts(self):
        for ci, system, machine in (("false", "Darwin", "arm64"), ("true", "Linux", "aarch64"), ("true", "Darwin", "x86_64")):
            with self.subTest(ci=ci, system=system, machine=machine), mock.patch.dict("preflight.os.environ", {"CI": ci}), mock.patch(
                    "preflight.platform.system", return_value=system), mock.patch("preflight.platform.machine", return_value=machine):
                with self.assertRaisesRegex(preflight.PreflightError, "requires_disposable_native_macos_arm64_ci"):
                    preflight.preflight(self.root / "unused.json", preflight.initial_report())


if __name__ == "__main__":
    unittest.main()
