// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package native

import (
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
	if len(entries[0].License) != 2 {
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
