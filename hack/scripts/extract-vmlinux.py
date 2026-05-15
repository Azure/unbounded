#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.
#
# extract-vmlinux.py -- decompress a kernel boot image (vmlinuz) to an ELF
# vmlinux on stdout. Mirrors the kernel's own scripts/extract-vmlinux logic:
# scans for the compression magic bytes (gzip, xz, bzip2, lz4, lzma, lzop,
# zstd), tries each candidate offset until one decompresses to something
# starting with the ELF magic.

import shutil
import subprocess
import sys

MAGICS = [
    (b"\x1f\x8b\x08",       ["gunzip"]),
    (b"\xfd7zXZ\x00",       ["xz", "-dc"]),
    (b"BZh",                ["bunzip2"]),
    (b"\x5d\x00\x00\x00",   ["lzma", "-dc"]),
    (b"\x89LZO\x00\r",      ["lzop", "-dc"]),
    (b"\x02!L\x18",         ["lz4", "-dc", "-l"]),
    (b"(\xb5/\xfd",         ["zstd", "-dc"]),
]

ELF_MAGIC = b"\x7fELF"


def try_decompress(blob: bytes, offset: int, cmd) -> bytes | None:
    if shutil.which(cmd[0]) is None:
        return None

    proc = subprocess.Popen(
        cmd,
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.DEVNULL,
    )
    out, _ = proc.communicate(blob[offset:])
    if not out.startswith(ELF_MAGIC):
        return None
    return out


def main() -> int:
    if len(sys.argv) != 2:
        print(f"usage: {sys.argv[0]} <vmlinuz>", file=sys.stderr)
        return 2

    with open(sys.argv[1], "rb") as f:
        blob = f.read()

    for magic, cmd in MAGICS:
        start = 0
        while True:
            idx = blob.find(magic, start)
            if idx < 0:
                break
            out = try_decompress(blob, idx, cmd)
            if out is not None:
                sys.stdout.buffer.write(out)
                return 0
            start = idx + 1

    print(f"{sys.argv[0]}: no recognized compression in {sys.argv[1]}",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
