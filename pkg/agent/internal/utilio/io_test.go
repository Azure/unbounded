// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package utilio

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallFileWithLimitedSizeRejectsOverflowingLimit(t *testing.T) {
	t.Parallel()

	path := t.TempDir() + "/installed"
	if err := InstallFileWithLimitedSize(path, strings.NewReader("content"), 0o600, math.MaxInt64); err == nil {
		t.Fatal("InstallFileWithLimitedSize error = nil")
	}
}

// TestWriteFileIfChanged covers the signal callers act on. A reapply that
// changes nothing must report false, or every reapply would look like a change
// and anything keyed on it, such as restarting a service, would fire each time.
func TestWriteFileIfChanged(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "nested", "config")

	changed, err := WriteFileIfChanged(path, []byte("one"), 0o644)
	if err != nil || !changed {
		t.Fatalf("first write: changed=%v err=%v, want changed=true", changed, err)
	}

	changed, err = WriteFileIfChanged(path, []byte("one"), 0o644)
	if err != nil || changed {
		t.Fatalf("identical rewrite: changed=%v err=%v, want changed=false", changed, err)
	}

	changed, err = WriteFileIfChanged(path, []byte("two"), 0o644)
	if err != nil || !changed {
		t.Fatalf("content change: changed=%v err=%v, want changed=true", changed, err)
	}

	data, err := os.ReadFile(path)
	if err != nil || string(data) != "two" {
		t.Fatalf("content = %q err=%v, want \"two\"", data, err)
	}

	// A drifted mode is deliberately not reported as changed. WriteFile
	// preserves existing permissions, so it could not be corrected here, and
	// reporting it would restart the file's reader on every call forever.
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	changed, err = WriteFileIfChanged(path, []byte("two"), 0o644)
	if err != nil || changed {
		t.Fatalf("mode drift: changed=%v err=%v, want changed=false", changed, err)
	}
}
