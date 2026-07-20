// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"testing"
)

// buildRRQ builds a TFTP read request packet: opcode 0x0001 then NUL-terminated
// filename, mode, and option/value pairs.
func buildRRQ(filename, mode string, opts ...string) []byte {
	out := []byte{0x00, 0x01}
	out = append(out, []byte(filename)...)
	out = append(out, 0)
	out = append(out, []byte(mode)...)
	out = append(out, 0)

	for _, s := range opts {
		out = append(out, []byte(s)...)
		out = append(out, 0)
	}

	return out
}

// blksizeOf returns the value of the (case-insensitive) blksize option in an
// RRQ, or "" if absent.
func blksizeOf(pkt []byte) string {
	fields := splitNulFields(pkt[2:])
	for i := 2; i+1 < len(fields); i += 2 {
		if bytes.EqualFold(fields[i], []byte("blksize")) {
			return string(fields[i+1])
		}
	}

	return ""
}

func TestClampTFTPBlksize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		pkt      []byte
		cap      int
		wantBlk  string // expected blksize option value after clamping
		wantSame bool   // whether the returned packet must be byte-identical
	}{
		{
			name:    "blksize above cap is clamped",
			pkt:     buildRRQ("shimx64.efi", "octet", "blksize", "1468"),
			cap:     1198,
			wantBlk: "1198",
		},
		{
			name:     "blksize at cap is untouched",
			pkt:      buildRRQ("grubx64.efi", "octet", "blksize", "1198"),
			cap:      1198,
			wantBlk:  "1198",
			wantSame: true,
		},
		{
			name:     "blksize below cap is untouched",
			pkt:      buildRRQ("vmlinuz", "octet", "blksize", "512"),
			cap:      1198,
			wantBlk:  "512",
			wantSame: true,
		},
		{
			name:     "rrq without blksize is untouched",
			pkt:      buildRRQ("initrd", "octet", "tsize", "0"),
			cap:      1198,
			wantBlk:  "",
			wantSame: true,
		},
		{
			name:    "blksize option name is case-insensitive",
			pkt:     buildRRQ("shimx64.efi", "octet", "BlkSize", "8192"),
			cap:     1198,
			wantBlk: "1198",
		},
		{
			name:    "cap below minimum is raised to 512",
			pkt:     buildRRQ("shimx64.efi", "octet", "blksize", "1468"),
			cap:     100,
			wantBlk: "512",
		},
	}

	for _, tc := range tests {
		tc := tc

		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			orig := append([]byte(nil), tc.pkt...)
			got := clampTFTPBlksize(tc.pkt, tc.cap)

			if tc.wantSame && !bytes.Equal(got, orig) {
				t.Fatalf("expected packet unchanged, got %v want %v", got, orig)
			}

			if gotBlk := blksizeOf(got); gotBlk != tc.wantBlk {
				t.Fatalf("blksize = %q, want %q", gotBlk, tc.wantBlk)
			}

			// The rebuilt packet must remain a valid RRQ (opcode preserved,
			// filename and mode intact).
			if got[0] != 0x00 || got[1] != 0x01 {
				t.Fatalf("opcode corrupted: %v", got[:2])
			}

			fields := splitNulFields(got[2:])
			if len(fields) < 2 {
				t.Fatalf("packet lost filename/mode: %v", fields)
			}
		})
	}
}

func TestClampTFTPBlksizeNonRRQ(t *testing.T) {
	t.Parallel()

	// DATA packet (opcode 0x0003) must be forwarded verbatim.
	data := []byte{0x00, 0x03, 0x00, 0x01, 0xde, 0xad, 0xbe, 0xef}

	got := clampTFTPBlksize(data, 1198)
	if !bytes.Equal(got, data) {
		t.Fatalf("non-RRQ packet altered: got %v want %v", got, data)
	}

	// Too-short packet is returned unchanged.
	short := []byte{0x00}

	got = clampTFTPBlksize(short, 1198)
	if !bytes.Equal(got, short) {
		t.Fatalf("short packet altered: got %v want %v", got, short)
	}
}
