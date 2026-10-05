// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
)

// TestCollectorRepositoryParity is an opt-in, read-only check against a populated
// checkout's NOTICE. Ordinary tests use hermetic fixtures and need no registry.
func TestCollectorRepositoryParity(t *testing.T) {
	root := os.Getenv("NOTICE_CARGO_REPO_ROOT")
	if root == "" {
		t.Skip("set NOTICE_CARGO_REPO_ROOT to check a populated checkout")
	}

	c := New()
	if err := c.Precheck(root); err != nil {
		t.Fatal(err)
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(root, "NOTICE"))
	if err != nil {
		t.Fatal(err)
	}

	var reference notice.Document
	if err := yaml.Unmarshal(data, &reference); err != nil {
		t.Fatal(err)
	}

	want := map[string]notice.Entry{}

	for _, entry := range reference.Notices {
		if entry.Ecosystem == c.Name() {
			want[entry.Dependency] = entry
		}
	}

	got := map[string]notice.Entry{}
	for _, entry := range entries {
		got[entry.Dependency] = entry
	}

	if len(want) == 0 || !reflect.DeepEqual(got, want) {
		t.Fatalf("Cargo entries differ: got %#v, want %#v", got, want)
	}

	t.Logf("all %d Cargo entries match %s, including licenses, links, and copyrights", len(got), filepath.Join(root, "NOTICE"))
}
