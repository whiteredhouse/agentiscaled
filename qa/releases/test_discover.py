import unittest
from unittest.mock import patch
import discover
from discover import pairs, version


class Discovery(unittest.TestCase):
    def test_every_intermediate_release_is_selected(self):
        tags = ['v' + release['version'] for release in discover.CATALOG['releases']]
        major, minor, patch_version = version(tags[-1])
        tags.extend(f'v{major}.{minor}.{patch_version + offset}' for offset in (1, 2))
        rows = [{'tag_name': t, 'draft': False, 'prerelease': False}
                for t in reversed(tags)]
        self.assertEqual(pairs(rows), list(zip(tags, tags[1:])))

    def test_draft_prerelease_and_nonversion_tags_ignored(self):
        tags = ['v' + release['version'] for release in discover.CATALOG['releases']]
        rows = [{'tag_name': tag, 'draft': False, 'prerelease': False}
                for tag in [*tags, 'nightly', 'v9999.0.1-rc1']]
        rows.append({'tag_name': 'v9999.0.1', 'draft': True, 'prerelease': False})
        self.assertEqual(pairs(rows), list(zip(tags, tags[1:])))

    def test_an_audited_release_cannot_be_satisfied_only_by_a_draft(self):
        rows = [{'tag_name': 'v' + release['version'], 'draft': False, 'prerelease': False}
                for release in discover.CATALOG['releases']]
        rows[-1]['draft'] = True
        with self.assertRaisesRegex(ValueError, 'omits required'):
            pairs(rows)

    def test_untrusted_tag_syntax_rejected(self):
        with self.assertRaises(ValueError):
            version('v1.2.3; command')


    def test_asset_replacement_invalidates_receipt(self):
        rows = [{'tag_name': 'v' + r['version'], 'draft': False, 'prerelease': False,
                 'assets': [{'id': 1, 'name': 'checksums.txt', 'updated_at': 'before'}]}
                for r in discover.CATALOG['releases']]
        with patch('discover.api', return_value=rows):
            before = discover.plan(True, False)['include'][-1]['key']
            rows[-1]['assets'][0]['updated_at'] = 'after'
            after = discover.plan(True, False)['include'][-1]['key']
        self.assertNotEqual(before, after)

    def test_compatibility_lane_keeps_same_inputs(self):
        matrix = discover.plan(False, True)['include']
        self.assertEqual(len(matrix), len(discover.CATALOG['releases']))
        self.assertTrue(all(entry['lane'] == 'baseline' for entry in matrix[:-1]))
        self.assertEqual(matrix[-1]['lane'], 'latest-homebrew')
        self.assertEqual(matrix[-2]['pins'], matrix[-1]['pins'])

    def test_pagination_is_not_silently_truncated(self):
        with patch('discover.api', return_value=[{}] * 100):
            with self.assertRaisesRegex(ValueError, 'pagination limit'):
                discover.plan(True, False)
