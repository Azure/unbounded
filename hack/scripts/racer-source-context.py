#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0
"""Copy tracked working-tree sources without walking scratch or other worktrees.

Uncommitted edits to tracked files are included. Add new sources to the index
before building images. Deleted files are omitted. No git objects or ignored
local artifacts are copied; Docker still applies the copied .dockerignore.
"""

import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile


def snapshot(root, destination):
    destination.mkdir()  # Refuse to overwrite any existing directory.
    paths = subprocess.check_output(['git', '-C', str(root), 'ls-files', '--cached', '-z'])
    for raw in paths.split(b'\0'):
        if not raw:
            continue
        relative = Path(os.fsdecode(raw))
        if relative.is_absolute() or '..' in relative.parts:
            raise ValueError(f'invalid tracked path: {relative}')
        source = root / relative
        # A tracked file under a locally replaced directory symlink must not
        # escape the source tree. Preserve leaf symlinks without following them.
        for parent in relative.parents:
            if parent != Path('.') and (root / parent).is_symlink():
                raise ValueError(f'tracked source has symlink parent: {relative}')
        if not source.exists() and not source.is_symlink():
            continue
        if not source.is_symlink() and source.is_dir():
            raise ValueError(f'submodules are not supported: {relative}')
        target = destination / relative
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target, follow_symlinks=False)


def main():
    root = Path(__file__).resolve().parents[2]
    if sys.argv[1] != '--build':
        snapshot(root, Path(sys.argv[1]))
        return 0
    scratch = root / 'tmp'
    scratch.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='racer-context-', dir=scratch) as directory:
        context = Path(directory) / 'source'
        snapshot(root, context)
        return subprocess.call(sys.argv[2:] + [str(context)])


if __name__ == '__main__':
    sys.exit(main())
