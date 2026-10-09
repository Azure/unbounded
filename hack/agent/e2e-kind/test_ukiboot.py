#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the PE parsing ukiboot uses to extend a UKI cmdline addon in place.

The disk-facing half needs qemu-nbd and a real image and is covered by the ACL
host in the e2e suite. A wrong header offset does not fail loudly: it writes
plausible bytes into an EFI executable that then will not boot.
"""
import os
import struct
import unittest
from pathlib import Path
from unittest.mock import patch

import ukiboot


def _pe(sections: list[tuple[str, int, int, int, int]], opt_size: int = 240,
        size_of_image: int = 0x100000) -> bytes:
    """Build a PE header with the given (name, vsize, vaddr, rsize, rptr) sections.

    Only the fields ukiboot reads are populated. The point is to be able to
    state the expected offsets independently of the code under test.
    """
    lfanew = 0x80
    out = bytearray(4096)
    out[0:2] = b"MZ"
    struct.pack_into("<I", out, 0x3C, lfanew)
    out[lfanew:lfanew + 4] = b"PE\x00\x00"
    struct.pack_into("<H", out, lfanew + 6, len(sections))
    struct.pack_into("<H", out, lfanew + 20, opt_size)
    struct.pack_into("<I", out, lfanew + 24 + 56, size_of_image)

    table = lfanew + 24 + opt_size
    for i, (name, vsize, vaddr, rsize, rptr) in enumerate(sections):
        entry = table + i * 40
        out[entry:entry + 8] = name.encode().ljust(8, b"\x00")
        struct.pack_into("<IIII", out, entry + 8, vsize, vaddr, rsize, rptr)

    return bytes(out)


class TestPESectionTable(unittest.TestCase):
    def test_sections_are_read_by_name_after_the_optional_header(self):
        """The optional header's size varies between PE32 and PE32+ and with
        the number of data directories, and the table follows it."""
        for opt_size in (224, 240):
            with self.subTest(opt_size=opt_size):
                header = _pe([
                    (".text", 0x10, 0x1000, 0x200, 0x400),
                    (".cmdline", 0x25, 0x2000, 0x200, 0x600),
                    (".linux", 0x30, 0x3000, 0x400, 0x800),
                ], opt_size=opt_size)
                sections = ukiboot.pe_sections(header)

                self.assertEqual(set(sections), {".text", ".cmdline", ".linux"})
                self.assertEqual(sections[".cmdline"][:4], (0x25, 0x2000, 0x200, 0x600))

    def test_vsize_offset_lands_on_the_right_field(self):
        """The one write ukiboot makes into a PE header, at the entry offset
        plus 8. The synthetic header gives .cmdline a VirtualSize of 5 and a
        VirtualAddress of 6, which are adjacent, so reading back 5 is what
        distinguishes a correct offset from one four bytes out."""
        header = _pe([
            (".text", 1, 2, 3, 4),
            (".cmdline", 5, 6, 7, 8),
        ])
        sections = ukiboot.pe_sections(header)
        offset = sections[".cmdline"][4] + 8

        self.assertEqual(struct.unpack_from("<I", header, offset)[0], 5)
        self.assertEqual(offset - (sections[".text"][4] + 8), 40, "sections are 40 bytes apart")


class TestAddonCapacity(unittest.TestCase):
    """An addon can only be extended in place because its .cmdline section's
    raw size is padded well past the string in it."""

    def test_room_is_measured_in_encoded_bytes_with_the_terminator(self):
        """A full section is rejected rather than truncated, which would drop
        the Ignition config URL. Counting characters would let a non-ASCII
        command line overrun the section."""
        cases = [
            (("a=1", "b=2", 1024), b"a=1 b=2"),
            (("x" * 1000, "y" * 100, 1024), None),
            (("", "x" * 16, 16), None),
            (("", "x" * 15, 16), b"x" * 15),
            (("", "\u00e9" * 8, 16), None),
            (("", "\u00e9" * 8, 17), "\u00e9".encode() * 8),
        ]
        for (current, extra, raw_size), want in cases:
            with self.subTest(extra=extra[:4], raw_size=raw_size):
                self.assertEqual(ukiboot.fit_cmdline(current, extra, raw_size), want)


class TestCmdlineRoom(unittest.TestCase):
    """VirtualSize grows with the command line, and the section must not reach
    into the next one in memory, whatever its raw size allows."""

    def test_room_is_bounded_by_raw_size_next_section_and_image_size(self):
        cases = [
            # The raw size is the tighter bound.
            ([(".cmdline", 0x10, 0x2000, 0x200, 0x400), (".linux", 0x10, 0x3000, 0x200, 0x600)], 0x100000, 0x200),
            # The next section in memory is closer than the raw size allows.
            ([(".cmdline", 0x10, 0x2000, 0x1000, 0x400), (".linux", 0x10, 0x2100, 0x200, 0x1400)], 0x100000, 0x100),
            # A section before it in memory does not bound it.
            ([(".text", 0x10, 0x1000, 0x200, 0x200), (".cmdline", 0x10, 0x2000, 0x200, 0x400)], 0x100000, 0x200),
            # The last section ends at SizeOfImage.
            ([(".cmdline", 0x10, 0x2000, 0x1000, 0x400)], 0x2080, 0x80),
        ]
        for sections, size_of_image, want in cases:
            with self.subTest(sections=sections, size_of_image=size_of_image):
                self.assertEqual(ukiboot.cmdline_room(_pe(sections, size_of_image=size_of_image)), want)


class TestSingleUKI(unittest.TestCase):
    def test_exactly_one_uki_is_required(self):
        """With more than one, the one patched may not be the one that boots."""
        self.assertEqual(ukiboot.single_uki(["vmlinuz.efi", "readme.txt"], Path("d")), "vmlinuz.efi")
        for names in ([], ["readme.txt"], ["a.efi", "b.EFI"]):
            with self.subTest(names=names):
                with self.assertRaises(RuntimeError):
                    ukiboot.single_uki(names, Path("d"))


class TestNbdServerStartup(unittest.TestCase):
    """A failed start must not leave the temporary directory behind, since
    __exit__ never runs for a constructor that raised."""

    def _start(self, popen):
        made = []
        real_mkdtemp = ukiboot.tempfile.mkdtemp

        def mkdtemp(**kw):
            made.append(real_mkdtemp(**kw))
            return made[-1]

        with patch.object(ukiboot.tempfile, "mkdtemp", side_effect=mkdtemp), \
                patch.object(ukiboot.subprocess, "Popen", side_effect=popen):
            with self.assertRaises((RuntimeError, FileNotFoundError)):
                ukiboot.NbdServer("image.qcow2")
        return made[0]

    def test_qemu_nbd_missing_cleans_up(self):
        def missing(*_args, **_kw):
            raise FileNotFoundError("qemu-nbd")

        self.assertFalse(os.path.exists(self._start(missing)))


if __name__ == "__main__":
    unittest.main()
