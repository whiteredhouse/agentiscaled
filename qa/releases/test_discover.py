import unittest
from unittest.mock import patch
import discover
from discover import pairs, version


class Discovery(unittest.TestCase):
    def test_every_intermediate_release_is_selected(self):
        rows = [{'tag_name': t, 'draft': False, 'prerelease': False}
                for t in ('v0.1.10', 'v0.1.11', 'v0.1.12', 'v0.1.13')]
        self.assertEqual(pairs(rows), [('v0.1.10','v0.1.11'), ('v0.1.11','v0.1.12'), ('v0.1.12','v0.1.13')])

    def test_draft_prerelease_and_nonversion_tags_ignored(self):
        rows = [{'tag_name': t, 'draft': False, 'prerelease': False} for t in ('v0.1.10','v0.1.11','nightly','v0.1.12-rc1')]
        rows.append({'tag_name':'v0.1.12','draft':True,'prerelease':False})
        self.assertEqual(pairs(rows), [('v0.1.10','v0.1.11')])

    def test_untrusted_tag_syntax_rejected(self):
        with self.assertRaises(ValueError):
            version('v1.2.3; command')


    def test_asset_replacement_invalidates_receipt(self):
        rows = [{'tag_name': 'v' + r['version'], 'draft': False, 'prerelease': False,
                 'assets': [{'id': 1, 'name': 'checksums.txt', 'updated_at': 'before'}]}
                for r in discover.BASE['releases']]
        with patch('discover.api', return_value=rows):
            before = discover.plan(True, False)['include'][0]['key']
            rows[-1]['assets'][0]['updated_at'] = 'after'
            after = discover.plan(True, False)['include'][0]['key']
        self.assertNotEqual(before, after)

    def test_compatibility_lane_keeps_same_inputs(self):
        matrix = discover.plan(False, True)['include']
        self.assertEqual([r['lane'] for r in matrix], ['baseline', 'latest-homebrew'])
        self.assertEqual(matrix[0]['pins'], matrix[1]['pins'])

    def test_pagination_is_not_silently_truncated(self):
        with patch('discover.api', return_value=[{}] * 100):
            with self.assertRaisesRegex(ValueError, 'pagination limit'):
                discover.plan(True, False)
