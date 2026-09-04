#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Boot a Unified Kernel Image disk under QEMU with a supplied Ignition config.

Azure Container Linux is a Flatcar-derived image: an EFI system partition holds
a UKI that shim and systemd-boot load, /usr is a dm-verity btrfs image mounted
read-only, and first-boot provisioning is Ignition rather than cloud-init. Two
things follow that the rest of the harness cannot express.

Getting a kernel argument in. QEMU has no way to append to the command line of
a UKI booted through systemd-boot, and the command line is where an Ignition
config source is named. Extracting the kernel and initrd from the UKI's PE
sections lets QEMU boot them directly with -kernel/-initrd/-append, which gives
the harness the whole command line and leaves the image untouched.

Getting a config in. The image ships its own Ignition config on the OEM
partition, which creates the `core` user and masks the Azure agent. Ignition
treats that as the *user* config, and a user config takes precedence over the
platform provider, so a config offered through fw_cfg, ignition.config.url or
Azure CustomData is never read. The supported way to add to it is a *base*
config: ignition-setup copies /oem/base/base.ign into /usr/lib/ignition/base.d/
and Ignition merges everything there underneath the user config. Writing into
the OEM partition would mean mounting a filesystem inside the disk image, which
needs root. Seeding the same path through an initramfs segment does not, and
the vendor's own config still applies.

Everything here is read-only with respect to the disk image and needs no
privileges: qemu-nbd serves the image over a unix socket and a minimal NBD
client reads it.
"""
from __future__ import annotations

import os
import re
import socket
import struct
import subprocess
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

# NBD protocol constants (fixed newstyle handshake).
NBD_OPT_GO = 7
NBD_REP_ACK = 1
NBD_REP_INFO = 3
NBD_INFO_EXPORT = 0
NBD_CMD_READ = 0
NBD_FLAG_C_FIXED_NEWSTYLE = 1
NBD_REQUEST_MAGIC = 0x25609513
NBD_SIMPLE_REPLY_MAGIC = 0x67446698
NBD_OPT_REPLY_MAGIC = 0x3E889045565A9
NBD_REP_ERROR_BIT = 0x80000000

EFI_SYSTEM_PARTITION_TYPE = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"

# cpio newc constants.
CPIO_MAGIC = b"070701"
CPIO_TRAILER = "TRAILER!!!"
S_IFDIR = 0o040000
S_IFREG = 0o100000


class NbdClient:
    """Minimal NBD reader: one export, random-access reads."""

    def __init__(self, sock_path: str):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(sock_path)
        self.size = self._handshake()
        self._handle = 0

    def _recv(self, n: int) -> bytes:
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise EOFError("NBD connection closed")
            buf += chunk
        return buf

    def _handshake(self) -> int:
        if self._recv(8) != b"NBDMAGIC":
            raise RuntimeError("not an NBD server")
        if self._recv(8) != b"IHAVEOPT":
            raise RuntimeError("server does not speak fixed newstyle NBD")
        self._recv(2)  # handshake flags
        self.sock.sendall(struct.pack(">I", NBD_FLAG_C_FIXED_NEWSTYLE))

        payload = struct.pack(">I", 0) + struct.pack(">H", 0)  # default export, no info requests
        self.sock.sendall(b"IHAVEOPT" + struct.pack(">II", NBD_OPT_GO, len(payload)) + payload)

        size = 0
        while True:
            magic, option, rep_type, length = struct.unpack(">QIII", self._recv(20))
            if magic != NBD_OPT_REPLY_MAGIC:
                raise RuntimeError(f"bad NBD option reply magic {magic:#x}")
            data = self._recv(length) if length else b""
            if rep_type == NBD_REP_INFO and len(data) >= 10:
                if struct.unpack(">H", data[:2])[0] == NBD_INFO_EXPORT:
                    size = struct.unpack(">Q", data[2:10])[0]
            elif rep_type == NBD_REP_ACK:
                return size
            elif rep_type & NBD_REP_ERROR_BIT:
                raise RuntimeError(f"NBD option {option} rejected ({rep_type:#x}): {data!r}")

    def read(self, offset: int, length: int) -> bytes:
        out = bytearray()
        while length > 0:
            n = min(length, 4 << 20)
            self._handle += 1
            self.sock.sendall(struct.pack(
                ">IHHQQI", NBD_REQUEST_MAGIC, 0, NBD_CMD_READ, self._handle, offset, n))
            magic, error, _handle = struct.unpack(">IIQ", self._recv(16))
            if magic != NBD_SIMPLE_REPLY_MAGIC:
                raise RuntimeError(f"bad NBD reply magic {magic:#x}")
            if error:
                raise RuntimeError(f"NBD read error {error} at offset {offset}")
            out += self._recv(n)
            offset += n
            length -= n
        return bytes(out)

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            pass


class NbdServer:
    """qemu-nbd serving a disk image read-only on a unix socket in a temp dir."""

    def __init__(self, image: str, image_format: str = "qcow2"):
        self._dir = tempfile.mkdtemp(prefix="ukiboot-")
        self.sock_path = os.path.join(self._dir, "nbd.sock")
        self.proc = subprocess.Popen(
            ["qemu-nbd", "--read-only", "--persistent",
             "--format", image_format, "--socket", self.sock_path, image],
            stdout=subprocess.DEVNULL, stderr=subprocess.PIPE,
        )
        deadline = time.time() + 15
        while time.time() < deadline:
            if os.path.exists(self.sock_path):
                return
            if self.proc.poll() is not None:
                err = self.proc.stderr.read().decode("utf-8", "replace") if self.proc.stderr else ""
                raise RuntimeError(f"qemu-nbd exited: {err}")
            time.sleep(0.05)
        self.close()
        raise RuntimeError("qemu-nbd did not create its socket in time")

    def close(self) -> None:
        self.proc.terminate()
        try:
            self.proc.wait(timeout=10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        for cleanup in (lambda: os.unlink(self.sock_path), lambda: os.rmdir(self._dir)):
            try:
                cleanup()
            except OSError:
                pass

    def __enter__(self) -> "NbdServer":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()


@dataclass(frozen=True)
class Partition:
    name: str
    type_guid: str
    first_lba: int
    last_lba: int

    @property
    def offset(self) -> int:
        return self.first_lba * 512


def read_partitions(dev: NbdClient) -> list[Partition]:
    header = dev.read(512, 512)
    if header[:8] != b"EFI PART":
        raise RuntimeError("disk has no GPT")
    entries_lba = struct.unpack_from("<Q", header, 0x48)[0]
    count = struct.unpack_from("<I", header, 0x50)[0]
    entry_size = struct.unpack_from("<I", header, 0x54)[0]
    table = dev.read(entries_lba * 512, count * entry_size)

    def guid(raw: bytes) -> str:
        d1, d2, d3 = struct.unpack_from("<IHH", raw, 0)
        rest = raw[8:16]
        return (f"{d1:08x}-{d2:04x}-{d3:04x}-{rest[0]:02x}{rest[1]:02x}-"
                + "".join(f"{b:02x}" for b in rest[2:]))

    out = []
    for i in range(count):
        entry = table[i * entry_size:(i + 1) * entry_size]
        if len(entry) < 128 or entry[:16] == b"\x00" * 16:
            continue
        out.append(Partition(
            name=entry[56:128].decode("utf-16-le").rstrip("\x00"),
            type_guid=guid(entry[:16]),
            first_lba=struct.unpack_from("<Q", entry, 32)[0],
            last_lba=struct.unpack_from("<Q", entry, 40)[0],
        ))
    return out


class Fat32:
    """Read-only FAT32 reader over an NBD-backed partition."""

    def __init__(self, dev: NbdClient, part_offset: int):
        self.dev = dev
        self.base = part_offset
        boot = self._read(0, 512)
        self.bytes_per_sector = struct.unpack_from("<H", boot, 11)[0]
        sectors_per_cluster = boot[13]
        reserved = struct.unpack_from("<H", boot, 14)[0]
        num_fats = boot[16]
        fat_sectors = struct.unpack_from("<I", boot, 36)[0]
        if not fat_sectors:
            raise RuntimeError("not a FAT32 filesystem")
        self.root_cluster = struct.unpack_from("<I", boot, 44)[0]
        self.data_offset = (reserved + num_fats * fat_sectors) * self.bytes_per_sector
        self.cluster_size = sectors_per_cluster * self.bytes_per_sector
        self._fat = self._read(reserved * self.bytes_per_sector,
                               fat_sectors * self.bytes_per_sector)

    def _read(self, offset: int, length: int) -> bytes:
        return self.dev.read(self.base + offset, length)

    def _cluster_runs(self, cluster: int) -> list[tuple[int, int]]:
        """Collapse a cluster chain into contiguous (offset, length) runs."""
        chain = []
        while 2 <= cluster < 0x0FFFFFF8:
            chain.append(cluster)
            cluster = struct.unpack_from("<I", self._fat, cluster * 4)[0] & 0x0FFFFFFF

        runs = []
        i = 0
        while i < len(chain):
            j = i
            while j + 1 < len(chain) and chain[j + 1] == chain[j] + 1:
                j += 1
            start = self.data_offset + (chain[i] - 2) * self.cluster_size
            runs.append((start, (j - i + 1) * self.cluster_size))
            i = j + 1
        return runs

    def read_file(self, cluster: int, size: int, offset: int = 0,
                  length: int | None = None) -> bytes:
        """Read a byte range of a file without materializing the whole file."""
        if length is None:
            length = size - offset
        length = max(0, min(length, size - offset))
        out = bytearray()
        pos = 0
        for run_start, run_len in self._cluster_runs(cluster):
            if len(out) >= length:
                break
            run_end = pos + run_len
            if run_end > offset:
                skip = max(0, offset - pos)
                take = min(run_len - skip, length - len(out))
                out += self._read(run_start + skip, take)
            pos = run_end
        return bytes(out)

    def list_dir(self, cluster: int) -> list[tuple[str, int, int, int]]:
        """Return (name, attributes, start cluster, size) for each entry."""
        data = b"".join(self._read(start, length) for start, length in self._cluster_runs(cluster))
        entries: list[tuple[str, int, int, int]] = []
        long_name: list[tuple[int, str]] = []
        for i in range(0, len(data), 32):
            entry = data[i:i + 32]
            if len(entry) < 32 or entry[0] == 0:
                break
            if entry[0] == 0xE5:
                long_name = []
                continue
            if entry[11] == 0x0F:
                text = (entry[1:11] + entry[14:26] + entry[28:32]).decode("utf-16-le", "ignore")
                long_name.append((entry[0] & 0x3F, text.split("\x00")[0]))
                continue
            if long_name:
                name = "".join(text for _, text in sorted(long_name))
            else:
                stem = entry[0:8].decode("ascii", "replace").rstrip()
                ext = entry[8:11].decode("ascii", "replace").rstrip()
                name = f"{stem}.{ext}" if ext else stem
            long_name = []
            start = ((struct.unpack_from("<H", entry, 20)[0] << 16)
                     | struct.unpack_from("<H", entry, 26)[0])
            entries.append((name, entry[11], start, struct.unpack_from("<I", entry, 28)[0]))
        return entries

    def lookup(self, path: str) -> tuple[int, int] | None:
        """Resolve a path to (start cluster, size). FAT lookups ignore case."""
        cluster = self.root_cluster
        parts = [p for p in path.split("/") if p]
        for index, part in enumerate(parts):
            for name, _attr, start, size in self.list_dir(cluster):
                if name.lower() != part.lower():
                    continue
                if index == len(parts) - 1:
                    return start, size
                cluster = start
                break
            else:
                return None
        return None

    def list_names(self, path: str) -> list[str]:
        found = self.lookup(path)
        if not found:
            return []
        return [name for name, _attr, _start, _size in self.list_dir(found[0])
                if name not in (".", "..")]


def pe_sections(header: bytes) -> dict[str, tuple[int, int, int, int]]:
    """Return {section: (virtual size, virtual address, raw size, raw pointer)}."""
    lfanew = struct.unpack_from("<I", header, 0x3C)[0]
    count = struct.unpack_from("<H", header, lfanew + 6)[0]
    opt_size = struct.unpack_from("<H", header, lfanew + 20)[0]
    table = lfanew + 24 + opt_size
    out = {}
    for i in range(count):
        entry = header[table + i * 40: table + (i + 1) * 40]
        out[entry[0:8].rstrip(b"\x00").decode()] = struct.unpack_from("<IIII", entry, 8)
    return out


def _section_bytes(fat: Fat32, cluster: int, size: int,
                   sections: dict[str, tuple[int, int, int, int]], name: str) -> bytes:
    vsize, _vaddr, rsize, rptr = sections[name]
    # A PE pads each section up to file alignment, so the raw size is rounded
    # up. The virtual size is the true payload length; writing the padding
    # would corrupt an initrd and confuse a command line.
    return fat.read_file(cluster, size, rptr, min(vsize, rsize) if vsize else rsize)


@dataclass(frozen=True)
class ExtractedUki:
    kernel: Path
    initrd: Path
    cmdline: str


def extract_uki(image: Path, out_dir: Path, image_format: str = "qcow2") -> ExtractedUki:
    """Extract the kernel, initrd and command line from the UKI on an image's ESP.

    The command line is assembled from the addons in the UKI's .extra.d
    directory the same way systemd-stub does, so it matches what the image
    would boot with.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    kernel_path = out_dir / "vmlinuz"
    initrd_path = out_dir / "initrd"

    with NbdServer(str(image), image_format) as server:
        dev = NbdClient(server.sock_path)
        try:
            esp = next((p for p in read_partitions(dev)
                        if p.type_guid == EFI_SYSTEM_PARTITION_TYPE), None)
            if esp is None:
                raise RuntimeError(f"{image} has no EFI system partition")
            fat = Fat32(dev, esp.offset)

            ukis = [n for n in fat.list_names("/EFI/Linux") if n.lower().endswith(".efi")]
            if not ukis:
                raise RuntimeError(f"{image} has no UKI under /EFI/Linux")
            uki_name = sorted(ukis)[0]

            found = fat.lookup(f"/EFI/Linux/{uki_name}")
            if found is None:
                raise RuntimeError(f"cannot open UKI {uki_name}")
            cluster, size = found

            sections = pe_sections(fat.read_file(cluster, size, 0, 8192))
            for required in (".linux", ".initrd"):
                if required not in sections:
                    raise RuntimeError(f"UKI {uki_name} has no {required} section")

            for section, dest in ((".linux", kernel_path), (".initrd", initrd_path)):
                vsize, _vaddr, rsize, rptr = sections[section]
                length = min(vsize, rsize) if vsize else rsize
                with dest.open("wb") as handle:
                    offset, remaining = rptr, length
                    while remaining > 0:
                        chunk = min(remaining, 8 << 20)
                        handle.write(fat.read_file(cluster, size, offset, chunk))
                        offset += chunk
                        remaining -= chunk

            parts = []
            if ".cmdline" in sections:
                parts.append(_section_bytes(fat, cluster, size, sections, ".cmdline"))

            addon_dir = f"/EFI/Linux/{uki_name}.extra.d"
            for addon in sorted(fat.list_names(addon_dir)):
                if not addon.lower().endswith(".efi"):
                    continue
                entry = fat.lookup(f"{addon_dir}/{addon}")
                if entry is None:
                    continue
                a_cluster, a_size = entry
                a_sections = pe_sections(fat.read_file(a_cluster, a_size, 0, min(a_size, 8192)))
                if ".cmdline" in a_sections:
                    parts.append(_section_bytes(fat, a_cluster, a_size, a_sections, ".cmdline"))
        finally:
            dev.close()

    text = " ".join(p.split(b"\x00")[0].decode("utf-8", "replace") for p in parts)
    return ExtractedUki(kernel=kernel_path, initrd=initrd_path,
                        cmdline=re.sub(r"\s+", " ", text).strip())


def strip_kernel_args(cmdline: str, *names: str) -> str:
    """Remove every occurrence of the given key=value arguments."""
    drop = set(names)
    return " ".join(arg for arg in cmdline.split()
                    if arg.split("=", 1)[0] not in drop)


def _cpio_entry(name: str, mode: int, data: bytes, ino: int) -> bytes:
    name_bytes = name.encode() + b"\0"
    fields = [ino, mode, 0, 0, 1, 0, len(data), 0, 0, 0, 0, len(name_bytes), 0]
    out = bytearray(CPIO_MAGIC + b"".join(b"%08X" % field for field in fields) + name_bytes)
    out += b"\0" * (-len(out) % 4)
    out += data
    out += b"\0" * (-len(out) % 4)
    return bytes(out)


def cpio_segment(files: dict[str, bytes], dirs: list[str] | None = None) -> bytes:
    """Build an uncompressed newc cpio archive.

    Parent directories are emitted explicitly because the kernel's initramfs
    unpacker does not create them implicitly. Re-creating a directory the base
    initramfs already has is harmless.
    """
    out = bytearray()
    ino = 0xC0DE0000
    for path in dirs or []:
        ino += 1
        out += _cpio_entry(path, S_IFDIR | 0o755, b"", ino)
    for path, data in files.items():
        ino += 1
        out += _cpio_entry(path, S_IFREG | 0o644, data, ino)
    ino += 1
    out += _cpio_entry(CPIO_TRAILER, 0, b"", ino)
    out += b"\0" * (-len(out) % 512)
    return bytes(out)


def seed_initrd(initrd: Path, files: dict[str, bytes], dest: Path,
                dirs: list[str] | None = None) -> Path:
    """Write an initrd with an extra uncompressed segment prepended, into `dest`.

    Order matters. The kernel walks concatenated initramfs archives and sniffs
    each one's compression; an uncompressed archive placed after the compressed
    initrd is read as a compressed archive with a bad magic, and the kernel
    reports "invalid magic at start of compressed archive" and drops it. Leading
    with the uncompressed segment is the layout the early microcode loader uses,
    and the one the kernel supports.
    """
    segment = cpio_segment(files=files, dirs=dirs)
    with dest.open("wb") as handle:
        handle.write(segment)
        handle.write(initrd.read_bytes())
    return dest


def seed_ignition_initrd(initrd: Path, config_json: str, dest: Path,
                         network_units: dict[str, str] | None = None,
                         name: str = "10-unbounded.ign") -> Path:
    """Write an initrd carrying an Ignition base config and optional networking.

    Ignition runs inside the initramfs, so anything it needs to reach has to be
    reachable from there. A network unit that Ignition itself writes lands in
    the real root and does not exist yet at that point, which leaves Ignition
    with whatever the image's own initramfs configures - DHCP, on an image built
    for a cloud. Seeding the unit into the initramfs as well is what lets
    Ignition fetch over a statically addressed network.
    """
    files = {f"usr/lib/ignition/base.d/{name}": config_json.encode()}
    dirs = ["usr", "usr/lib", "usr/lib/ignition", "usr/lib/ignition/base.d"]

    if network_units:
        dirs.append("usr/lib/systemd")
        dirs.append("usr/lib/systemd/network")
        for unit_name, contents in network_units.items():
            files[f"usr/lib/systemd/network/{unit_name}"] = contents.encode()

    return seed_initrd(initrd, files, dest, dirs)


def main() -> None:
    import argparse

    parser = argparse.ArgumentParser(description=__doc__.split("\n", maxsplit=1)[0])
    parser.add_argument("image", type=Path)
    parser.add_argument("out_dir", type=Path)
    parser.add_argument("--ignition", type=Path, help="Ignition config to seed as a base config")
    args = parser.parse_args()

    result = extract_uki(args.image, args.out_dir)
    print(f"kernel:  {result.kernel} ({result.kernel.stat().st_size} bytes)")
    print(f"initrd:  {result.initrd} ({result.initrd.stat().st_size} bytes)")
    print(f"cmdline: {result.cmdline}")

    if args.ignition:
        seeded = seed_ignition_initrd(result.initrd, args.ignition.read_text(),
                                      args.out_dir / "initrd.seeded")
        print(f"seeded:  {seeded} ({seeded.stat().st_size} bytes)")


if __name__ == "__main__":
    main()
