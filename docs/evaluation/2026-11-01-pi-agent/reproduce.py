#!/usr/bin/env python3
"""Run the pinned upstream Pi research tests without changing this repository."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import urllib.request

REVISION = '2c8d1feb147f6b3d97e855da803bfe7b19f3c403'
CATALOG = 'sha256-7fe1b41feaf43a7aad4bc832e6b306c7ced56e6d41bbc724b74e91eeacff6edf'


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    source = Path(__file__).resolve().parent
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    # Research only: these selected tests use faux streams, not provider accounts.
    for name in list(env):
        if any(word in name.upper() for word in ('API_KEY', 'TOKEN', 'SECRET', 'CREDENTIAL')):
            env.pop(name, None)
    env['PI_OFFLINE'] = '1'

    def command(argv, cwd, log, timeout=300):
        with (output / log).open('w') as handle:
            subprocess.run(argv, cwd=cwd, env=env, stdout=handle,
                           stderr=subprocess.STDOUT, check=True, timeout=timeout)

    with tempfile.TemporaryDirectory(prefix='harness-pi-research-') as temporary:
        root = Path(temporary).resolve()
        repo = root / 'pi'
        deps = root / 'deps'
        repo.mkdir()
        deps.mkdir()
        command(['git', 'init'], repo, 'git-init.log')
        command(['git', 'remote', 'add', 'origin', 'https://github.com/earendil-works/pi.git'], repo, 'git-remote.log')
        command(['git', 'fetch', '--depth', '1', 'origin', REVISION], repo, 'git-fetch.log')
        command(['git', 'checkout', '--detach', 'FETCH_HEAD'], repo, 'git-checkout.log')
        actual = subprocess.check_output(['git', 'rev-parse', 'HEAD'], cwd=repo, text=True).strip()
        if actual != REVISION:
            raise RuntimeError('Pi revision mismatch')
        shutil.copyfile(source / 'test-deps.package.json', deps / 'package.json')
        shutil.copyfile(source / 'test-deps.package-lock.json', deps / 'package-lock.json')
        command(['npm', 'ci', '--prefix', str(deps), '--ignore-scripts', '--no-audit', '--no-fund',
                 '--registry=https://registry.npmjs.org/', '--fetch-timeout=15000', '--fetch-retries=1'], root, 'npm-ci.log')
        (repo / 'node_modules').symlink_to(deps / 'node_modules', target_is_directory=True)
        namespace = deps / 'node_modules/@earendil-works'
        namespace.mkdir(exist_ok=True)
        (namespace / 'chord').symlink_to(repo / 'packages/chord', target_is_directory=True)
        catalog = root / 'models.all.json'
        url = 'https://pi.dev/api/models/revisions/' + CATALOG + '?types=chat,image,classifier'
        with urllib.request.urlopen(url, timeout=30) as response:
            data = response.read(20 * 1024 * 1024 + 1)
        if len(data) > 20 * 1024 * 1024 or hashlib.sha256(data).hexdigest() != CATALOG.removeprefix('sha256-'):
            raise RuntimeError('Pi model catalog hash/size mismatch')
        catalog.write_bytes(data)
        command(['node', 'packages/ai/scripts/hydrate-model-catalog.ts', str(catalog)], repo, 'hydrate.log')
        shutil.copyfile(source / 'vitest.config.ts.txt', repo / 'research-vitest.config.ts')
        shutil.copyfile(source / 'session-probe.test.ts.txt', repo / 'research-session-probe.test.ts')
        vitest = str(deps / 'node_modules/.bin/vitest')
        command([vitest, 'run', '--config', 'packages/agent/vitest.config.ts',
                 'packages/agent/test/agent-loop.test.ts', 'packages/agent/test/agent.test.ts'], repo, 'agent.log')
        command([vitest, 'run', '--config', 'research-vitest.config.ts',
                 'packages/durable/test/memory-storage.test.ts', 'packages/durable/test/session-forks.test.ts',
                 'packages/durable/test/harness-tools-recovery.test.ts', 'packages/durable/test/harness-ownership.test.ts'], repo, 'durable.log')
        command([vitest, 'run', '--config', 'research-vitest.config.ts',
                 'packages/durable/test/env-node-conformance.test.ts'], repo, 'env-node.log')
        command([vitest, 'run', '--config', 'research-vitest.config.ts',
                 'research-session-probe.test.ts'], repo, 'session-probe.log')
        command(['git', 'diff', '--exit-code'], repo, 'source-clean.log')
        (output / 'revision.json').write_text(json.dumps({
            'pi_revision': actual, 'catalog_revision': CATALOG,
            'node': subprocess.check_output(['node', '--version'], text=True).strip(),
            'scope': 'selected controlled-stream, recovery and local filesystem tests; not full CLI, remote daemon or live-model validation',
        }, indent=2) + '\n')
    print('PASS: ' + str(output))


if __name__ == '__main__':
    main()
