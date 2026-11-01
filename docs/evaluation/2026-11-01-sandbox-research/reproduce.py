#!/usr/bin/env python3
"""Reproduce the bounded research probe inside the private Docker fixture."""
import argparse
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    source = Path(__file__).resolve()
    repo = source.parents[3]
    with tempfile.TemporaryDirectory(prefix='harness-research-probe-') as folder:
        probe = Path(folder) / 'probe.go'
        shutil.copyfile(source.with_name('probe.go.txt'), probe)
        subprocess.run(['python3', str(repo / 'scripts/verify-sandbox.py'),
                        '--probe', str(probe), '--output', str(args.output.resolve())],
                       cwd=repo, check=True)


if __name__ == '__main__':
    main()
