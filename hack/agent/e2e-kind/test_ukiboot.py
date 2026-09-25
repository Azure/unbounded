#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Tests for the PE parsing ukiboot uses to extend a UKI cmdline addon in place.

The disk-facing half of ukiboot needs qemu-nbd and a real image and is covered
by running the ACL host in the e2e suite. What is worth pinning here is the
header arithmetic underneath it, because a wrong offset does not fail loudly:
it writes plausible bytes into the wrong part of an EFI executable, and the
first sign of trouble is a guest that will not boot.
"""
import io
import os
import struct
import unittest
from pathlib import Path
from unittest.mock import patch

import ukiboot


def _pe(sections: list[tuple[str, int, int, int, int]], opt_size: int = 240) -> bytes:
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

    table = lfanew + 24 + opt_size
    for i, (name, vsize, vaddr, rsize, rptr) in enumerate(sections):
        entry = table + i * 40
        out[entry:entry + 8] = name.encode().ljust(8, b"\x00")
        struct.pack_into("<IIII", out, entry + 8, vsize, vaddr, rsize, rptr)

    return bytes(out)


class TestPESectionTable(unittest.TestCase):
    def test_offset_follows_the_optional_header(self):
        """The section table sits after the optional header, whose size varies.

        PE32 and PE32+ differ here, and so do images built with different
        numbers of data directories. Hardcoding either would read the table at
        the wrong place on exactly the images not tested against.
        """
        for opt_size in (224, 240):
            with self.subTest(opt_size=opt_size):
                header = _pe([(".text", 1, 2, 3, 4)], opt_size=opt_size)
                table, count, got_opt = ukiboot.pe_section_table_offset(header)

                self.assertEqual(table, 0x80 + 24 + opt_size)
                self.assertEqual(count, 1)
                self.assertEqual(got_opt, opt_size)

    def test_sections_are_read_by_name(self):
        header = _pe([
            (".text", 0x10, 0x1000, 0x200, 0x400),
            (".cmdline", 0x25, 0x2000, 0x200, 0x600),
            (".linux", 0x30, 0x3000, 0x400, 0x800),
        ])
        sections = ukiboot.pe_sections(header)

        self.assertEqual(set(sections), {".text", ".cmdline", ".linux"})
        self.assertEqual(sections[".cmdline"], (0x25, 0x2000, 0x200, 0x600))

    def test_section_names_are_nul_stripped(self):
        """PE names are a fixed eight bytes, NUL padded rather than terminated."""
        header = _pe([(".cmdline", 1, 2, 3, 4)])
        self.assertIn(".cmdline", ukiboot.pe_sections(header))

    def test_index_locates_the_entry_to_rewrite(self):
        header = _pe([
            (".text", 1, 2, 3, 4),
            (".cmdline", 5, 6, 7, 8),
        ])
        self.assertEqual(ukiboot._pe_section_index(header, ".cmdline"), 1)

    def test_vsize_offset_lands_on_the_right_field(self):
        """The one write ukiboot makes into a PE header.

        The synthetic header gives .cmdline a VirtualSize of 5 and a
        VirtualAddress of 6, which are adjacent. Reading back 5 through the
        computed offset is what distinguishes a correct offset from one that is
        four bytes out and would overwrite the address instead.
        """
        header = _pe([
            (".text", 1, 2, 3, 4),
            (".cmdline", 5, 6, 7, 8),
        ])
        offset = ukiboot.pe_section_vsize_offset(header, ".cmdline")

        self.assertEqual(struct.unpack_from("<I", header, offset)[0], 5)

        # And the first section's field, to show the index is being applied
        # rather than the first entry always being chosen.
        first = ukiboot.pe_section_vsize_offset(header, ".text")
        self.assertEqual(struct.unpack_from("<I", header, first)[0], 1)
        self.assertEqual(offset - first, 40, "sections are 40 bytes apart")

    def test_missing_section_is_an_error(self):
        """A UKI without .cmdline cannot be patched, and must say so.

        Returning a default would write the command line into whatever section
        happened to be first.
        """
        header = _pe([(".text", 1, 2, 3, 4)])
        with self.assertRaises(RuntimeError):
            ukiboot._pe_section_index(header, ".cmdline")


class TestAddonCapacity(unittest.TestCase):
    """The room an addon's .cmdline section has for the addition.

    An addon can only be extended without reallocating because its .cmdline
    section's raw size is padded well past the string in it.
    """

    def test_room_is_measured_against_the_raw_size(self):
        self.assertEqual(ukiboot.fit_cmdline("a=1", "b=2", raw_size=1024), b"a=1 b=2")

    def test_a_full_section_is_rejected(self):
        """Rejected rather than truncated: a silently shortened kernel command
        line would drop the Ignition config URL and boot a host that provisions
        itself from nothing."""
        self.assertIsNone(ukiboot.fit_cmdline("x" * 1000, "y" * 100, raw_size=1024))

    def test_the_terminator_is_counted(self):
        """The NUL has to fit too, so a merge that exactly fills the section is
        one byte too long."""
        self.assertIsNone(ukiboot.fit_cmdline("", "x" * 16, raw_size=16))
        self.assertIsNotNone(ukiboot.fit_cmdline("", "x" * 15, raw_size=16))

    def test_size_is_counted_in_encoded_bytes(self):
        """Counting characters would let a non-ASCII command line overrun the
        section into whatever follows it."""
        self.assertIsNone(ukiboot.fit_cmdline("", "\u00e9" * 8, raw_size=16))
        self.assertEqual(ukiboot.fit_cmdline("", "\u00e9" * 8, raw_size=17), "\u00e9".encode() * 8)


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

    def test_qemu_nbd_exiting_cleans_up(self):
        class Exited:
            stderr = io.BytesIO(b"cannot open image")
            returncode = 1

            def poll(self):
                return 1

            def terminate(self):
                pass

            def wait(self, timeout=None):
                return 1

        self.assertFalse(os.path.exists(self._start(lambda *a, **kw: Exited())))

    def test_qemu_nbd_missing_cleans_up(self):
        def missing(*_args, **_kw):
            raise FileNotFoundError("qemu-nbd")

        self.assertFalse(os.path.exists(self._start(missing)))

if __name__ == "__main__":
    unittest.main()
