"""Aggregate fresh native reports; an omitted or skipped target never passes."""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess

PLATFORMS = ('darwin/amd64', 'darwin/arm64', 'linux/amd64', 'linux/arm64')


def check_pair(directory):
    pins = json.loads((directory / 'pins.json').read_text())
    versions = [release['version'] for release in pins['releases']]
    artifacts = json.loads((directory / 'release-artifacts.json').read_text())
    if not artifacts['success'] or len(artifacts['assets']) != 16:
        raise ValueError('incomplete artifact verification')
    if len(list((directory / 'attestations').rglob('*.attestation.json'))) != 18:
        raise ValueError('incomplete provenance records')
    for platform in PLATFORMS:
        system, arch = platform.split('/')
        consumer = json.loads((directory / f'release-{system}-{arch}.json').read_text())
        lifecycle = json.loads((directory / f'lifecycle-{system}-{arch}.json').read_text())
        for report in (consumer, lifecycle):
            if not report['success'] or (report['system'], report['arch']) != (system, arch):
                raise ValueError('missing/mismatched native platform: ' + platform)
        if [op['version'] for op in consumer['homebrew']['operations']] != versions:
            raise ValueError('missing Homebrew install/upgrade')
        if system == 'linux' and {p['format'] for p in consumer['packages'] if p['install_upgrade'] == 'passed'} != {'deb', 'rpm'}:
            raise ValueError('missing native Linux packages')
        observations = lifecycle['observations']
        if [o['version'] for o in observations] != [versions[0], *versions]:
            raise ValueError('missing service restart/upgrade observations')
        if any(o['proc_exe_sha256'] != o['expected_release_exe_sha256'] for o in observations):
            raise ValueError('loaded service image differs from verified release')
        if lifecycle['identity_before'] != lifecycle['identity_after'] or lifecycle['cleanup'] != 'passed':
            raise ValueError('identity preservation or service cleanup failed')
        if len(lifecycle['mesh_evidence']['enrollments']) != 1:
            raise ValueError('restart/upgrade re-enrolled the node')
    return pins


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--directory', type=Path, required=True)
    args = parser.parse_args()
    baseline = json.loads(Path(__file__).with_name('pins.json').read_text())['releases'][-1]['version']
    folders = sorted((args.directory / 'pairs').iterdir())
    if not folders:
        raise ValueError('no completed release pairs')
    for folder in folders:
        check_pair(folder)
    # The baseline stays directly addressable by downstream evidence consumers;
    # all later discovered pairs remain in the artifact and are mandatory too.
    reference = args.directory / 'pairs' / baseline
    check_pair(reference)
    for entry in reference.iterdir():
        target = args.directory / entry.name
        if entry.is_dir():
            shutil.copytree(entry, target)
        else:
            shutil.copy2(entry, target)
    proof = {'schema_version': 1, 'repository': 'whiteredhouse/agentiscaled',
             'workflow': '.github/workflows/release-verification.yml',
             'verifier_revision': subprocess.check_output(['git', 'rev-parse', 'HEAD'], text=True).strip(),
             'run_id': int(os.environ['GITHUB_RUN_ID']), 'run_attempt': int(os.environ['GITHUB_RUN_ATTEMPT']),
             'platforms': list(PLATFORMS), 'pairs': [p.name for p in folders],
             'success': True, 'cache_reused': False}
    (args.directory / 'proof.json').write_text(json.dumps(proof, indent=2) + '\n')


if __name__ == '__main__':
    main()
