"""Plan every stable release since the audited baseline, not just latest."""
import argparse
import base64
import copy
from functools import lru_cache
import hashlib
import json
import re
import subprocess
import urllib.request
from pathlib import Path

BASE = json.loads(Path(__file__).with_name('pins.json').read_text())
REPO = BASE['repository']
TAP = BASE['tap_repository']


def api(path):
    return json.loads(subprocess.check_output(['gh', 'api', path], text=True, timeout=60))


def version(tag):
    match = re.fullmatch(r'v(\d+)\.(\d+)\.(\d+)', tag)
    if not match:
        raise ValueError('not a stable semantic version tag')
    return tuple(map(int, match.groups()))


def pairs(releases):
    stable = sorted({r['tag_name'] for r in releases if not r['draft'] and not r['prerelease']
                     and re.fullmatch(r'v\d+\.\d+\.\d+', r['tag_name'])}, key=version)
    minimum = version('v' + BASE['releases'][-1]['version'])
    result = [(stable[i-1], tag) for i, tag in enumerate(stable) if i and version(tag) >= minimum]
    if not result:
        raise ValueError('no upgrade pair found at/after the audited baseline')
    if len(result) > 200:
        raise ValueError('release history exceeds CI matrix budget; advance audited baseline explicitly')
    return result


@lru_cache(maxsize=4096)
def source_file(repo, commit, path):
    data = api(f'repos/{repo}/contents/{path}?ref={commit}')
    return base64.b64decode(data['content'])


def resolve(tag, tap_commits):
    known = next((r for r in BASE['releases'] if 'v' + r['version'] == tag), None)
    if known:
        return copy.deepcopy(known)
    obj = api(f'repos/{REPO}/git/ref/tags/{tag}')['object']
    for _ in range(4):
        if obj['type'] == 'commit':
            break
        if obj['type'] != 'tag':
            raise ValueError('release ref is not a commit/tag')
        obj = api(f'repos/{REPO}/git/tags/{obj["sha"]}')['object']
    commit = obj['sha']
    if obj['type'] != 'commit' or not re.fullmatch(r'[0-9a-f]{40}', commit):
        raise ValueError('invalid source commit')
    if api(f'repos/{REPO}/compare/{commit}...main')['status'] not in ('ahead', 'identical'):
        raise ValueError('release commit is not on main')
    # This digest becomes trusted only when verify.py checks the signed
    # provenance against this exact tag/source/workflow before consumption.
    url = f'https://github.com/{REPO}/releases/download/{tag}/checksums.txt'
    with urllib.request.urlopen(url, timeout=60) as response:
        manifest = response.read(65537)
    if len(manifest) > 65536:
        raise ValueError('oversized manifest')
    tap_commit = None
    for sha in tap_commits:
        text = source_file(TAP, sha, 'Casks/agentiscaled.rb').decode()
        if f'version "{tag[1:]}"' in text:
            tap_commit = sha
            break
    if not tap_commit:
        raise ValueError(f'published tap revision missing for {tag}; retry after publisher completes')
    return {'version': tag[1:], 'commit': commit, 'tap_commit': tap_commit,
            'manifest_sha256': hashlib.sha256(manifest).hexdigest(),
            'license_sha256': hashlib.sha256(source_file(REPO, commit, 'LICENSE')).hexdigest(),
            'notice_sha256': hashlib.sha256(source_file(REPO, commit, 'NOTICE')).hexdigest()}


def plan(automatic, compatibility):
    if automatic:
        releases = []
        for page in range(1, 11):
            rows = api(f'repos/{REPO}/releases?per_page=100&page={page}')
            releases.extend(rows)
            if len(rows) < 100:
                break
        else:
            raise ValueError('release pagination limit exceeded; refusing incomplete coverage')
        selected = pairs(releases)
        needed = {tag for pair in selected for tag in pair}
        unknown = needed - {'v' + r['version'] for r in BASE['releases']}
        commits = []
        if unknown:
            for page in range(1, 11):
                rows = api(f'repos/{TAP}/commits?path=Casks/agentiscaled.rb&per_page=100&page={page}')
                commits.extend(r['sha'] for r in rows)
                if len(rows) < 100:
                    break
            else:
                raise ValueError('tap history pagination limit exceeded; advance audited baseline explicitly')
        resolved = {tag: resolve(tag, commits) for tag in needed}
        configurations = []
        metadata = {r['tag_name']: r for r in releases}
        for before, after in selected:
            pins = copy.deepcopy(BASE)
            pins['releases'] = [resolved[before], resolved[after]]
            pins['asset_revisions'] = {
                tag: hashlib.sha256(json.dumps(sorted(
                    [{k: a.get(k) for k in ('id', 'name', 'size', 'digest', 'updated_at')}
                     for a in metadata[tag].get('assets', [])], key=lambda a: a['name']),
                    sort_keys=True).encode()).hexdigest() for tag in (before, after)}
            configurations.append(pins)
    else:
        configurations = [copy.deepcopy(BASE)]
    entries = []
    for pins in configurations:
        digest = hashlib.sha256(json.dumps(pins, sort_keys=True).encode()).hexdigest()
        entries.append({'key': digest, 'pins': pins, 'lane': 'baseline', 'version': pins['releases'][-1]['version']})
    if compatibility:
        entries.append({**entries[-1], 'lane': 'latest-homebrew'})
    return {'include': entries}


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--automatic', action='store_true')
    parser.add_argument('--compatibility', action='store_true')
    args = parser.parse_args()
    matrix = json.dumps(plan(args.automatic, args.compatibility), separators=(',', ':'))
    # GitHub job outputs count UTF-16 bytes toward a 1 MB cap. Leave headroom
    # for latest_pins and other outputs; fail clearly rather than losing jobs.
    if len(matrix.encode('utf-16-le')) > 900000:
        raise SystemExit('release plan exceeds job output budget; advance audited baseline explicitly')
    print(matrix)
