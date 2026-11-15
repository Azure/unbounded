// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package inventory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// installFakeUdevadm writes a fake udevadm script into a temp dir and makes it
// the only PATH entry so readDiskSerial finds it instead of the real tool.
func installFakeUdevadm(t *testing.T, script string) {
	t.Helper()

	dir := t.TempDir()

	path := filepath.Join(dir, "udevadm")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake udevadm: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestReadDiskSerial_ParsesSerial(t *testing.T) {
	installFakeUdevadm(t, "#!/bin/sh\necho 'ID_SERIAL=ACME_SN12345'\necho 'ID_SERIAL_SHORT=SN12345'\n")

	if got := readDiskSerial(context.Background(), "sda"); got != "SN12345" {
		t.Fatalf("readDiskSerial = %q, want %q", got, "SN12345")
	}
}

func TestReadDiskSerial_MissingUdevadmFallsBack(t *testing.T) {
	// Empty PATH: the lookup fails and the collector keeps the empty serial.
	t.Setenv("PATH", t.TempDir())

	if got := readDiskSerial(context.Background(), "sda"); got != "" {
		t.Fatalf("readDiskSerial = %q, want empty fallback", got)
	}
}

func TestReadDiskSerial_HonorsContext(t *testing.T) {
	// The fake blocks far longer than the collection budget, so only context
	// cancellation can make the call return promptly. exec replaces the shell
	// with sleep so killing the process also closes the output pipe.
	installFakeUdevadm(t, "#!/bin/sh\nexec sleep 30\n")

	tests := []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
	}{
		{
			name: "expired deadline",
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
		},
		{
			name: "canceled context",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()

				return ctx, cancel
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := tc.ctx()
			defer cancel()

			start := time.Now()
			got := readDiskSerial(ctx, "sda")
			elapsed := time.Since(start)

			if got != "" {
				t.Errorf("readDiskSerial = %q, want empty fallback", got)
			}

			if elapsed > 5*time.Second {
				t.Fatalf("readDiskSerial blocked for %v instead of honoring the context", elapsed)
			}
		})
	}
}
