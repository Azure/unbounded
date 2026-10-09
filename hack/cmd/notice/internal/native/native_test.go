// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package native

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectorCollectHermetic(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"Makefile": `LIBFABRIC_VERSION ?= 2.5.1
OPENSSL_VERSION ?= 3.5.1
`,
	})

	entries, err := New().Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if got := entries[0].License[0].Link; got != "https://github.com/ofiwg/libfabric/blob/v2.5.1/COPYING" {
		t.Errorf("libfabric link = %q", got)
	}

	if len(entries[0].License) != 1 {
		t.Errorf("libfabric licenses = %#v", entries[0].License)
	}

	if got := entries[1].License[0].Link; got != "https://github.com/openssl/openssl/blob/openssl-3.5.1/LICENSE.txt" {
		t.Errorf("OpenSSL link = %q", got)
	}
}

func TestCollectorRejectsMissingPin(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{"Makefile": "OPENSSL_VERSION ?= 3.5.1\n"})

	if _, err := New().Collect(root); err == nil {
		t.Fatal("expected missing pin error")
	}
}

func TestCollectorAbsentPins(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{"Makefile": "# LIBFABRIC_VERSION ?= 2.5.1\nOTHER_VERSION = 1\n"})

	if err := New().Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := New().Collect(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v; want no entries and no error", entries, err)
	}
}

func TestCollectorRejectsInvalidPins(t *testing.T) {
	for name, data := range map[string]string{
		"missing OpenSSL":        "LIBFABRIC_VERSION ?= 2.5.1\n",
		"empty":                  "LIBFABRIC_VERSION ?=\nOPENSSL_VERSION :=\n",
		"malformed":              "LIBFABRIC_VERSION broken\nOPENSSL_VERSION broken\n",
		"unsupported assignment": "LIBFABRIC_VERSION += 2.5.1\n",
		"expression":             "LIBFABRIC_VERSION = $(VERSION)\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{"Makefile": data})

			if _, err := New().Collect(root); err == nil {
				t.Fatal("expected pin error")
			}
		})
	}
}

func TestMakeVersionsAssignments(t *testing.T) {
	for _, operator := range []string{"?=", ":=", "="} {
		t.Run(operator, func(t *testing.T) {
			versions, err := makeVersions("LIBFABRIC_VERSION"+operator+"2.5.1 # comment\nOPENSSL_VERSION "+operator+" 3.5.1\nLIBFABRIC_VERSION_EXTRA = ignored\n", "LIBFABRIC_VERSION", "OPENSSL_VERSION")
			if err != nil || len(versions) != 2 || versions["LIBFABRIC_VERSION"] != "2.5.1" || versions["OPENSSL_VERSION"] != "3.5.1" {
				t.Fatalf("makeVersions = %v, %v", versions, err)
			}
		})
	}
}

func TestMakeVersionsScannerError(t *testing.T) {
	if _, err := makeVersions(strings.Repeat("x", 128*1024), "LIBFABRIC_VERSION"); err == nil {
		t.Fatal("expected scanner error")
	}
}

func TestCollectorMakefileErrors(t *testing.T) {
	root := t.TempDir()
	if err := New().Precheck(root); err == nil {
		t.Fatal("expected missing Makefile error")
	}

	if _, err := New().Collect(root); err == nil {
		t.Fatal("expected missing Makefile error")
	}

	if err := os.Mkdir(filepath.Join(root, "Makefile"), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := New().Collect(root); err == nil {
		t.Fatal("expected Makefile read error")
	}
}

func TestCollectorWithoutPinnedNativeSources(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{"Makefile": "all:\n"})

	entries, err := New().Collect(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v; want no native entries", entries, err)
	}
}
