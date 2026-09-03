// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package oci_test

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/oci"
)

func TestParseV2Path(t *testing.T) {
	cases := []struct {
		name     string
		path     string
		wantRepo string
		wantKind ifaces.OriginRefKind
		wantRef  string
		wantOK   bool
	}{
		{
			name:     "simple manifest by tag",
			path:     "/v2/library/nginx/manifests/latest",
			wantRepo: "library/nginx",
			wantKind: ifaces.KindManifest,
			wantRef:  "latest",
			wantOK:   true,
		},
		{
			name:     "blob by digest",
			path:     "/v2/library/nginx/blobs/sha256:abc123",
			wantRepo: "library/nginx",
			wantKind: ifaces.KindBlob,
			wantRef:  "sha256:abc123",
			wantOK:   true,
		},
		{
			name:     "deep repo path",
			path:     "/v2/a/b/c/d/manifests/v1",
			wantRepo: "a/b/c/d",
			wantKind: ifaces.KindManifest,
			wantRef:  "v1",
			wantOK:   true,
		},
		{
			name:     "repo with manifests-substring uses last separator",
			path:     "/v2/cdn/manifests-mirror/foo/manifests/sha256:def",
			wantRepo: "cdn/manifests-mirror/foo",
			wantKind: ifaces.KindManifest,
			wantRef:  "sha256:def",
			wantOK:   true,
		},
		{
			// Repo component literally named "manifests" preceding a
			// /blobs/ separator: the rightmost separator (blobs) must win.
			name:     "manifests-named component before blobs separator",
			path:     "/v2/acme/manifests/cache/blobs/sha256:abc",
			wantRepo: "acme/manifests/cache",
			wantKind: ifaces.KindBlob,
			wantRef:  "sha256:abc",
			wantOK:   true,
		},
		{
			// Symmetric case: repo component named "blobs" preceding a
			// /manifests/ separator; rightmost separator (manifests) wins.
			name:     "blobs-named component before manifests separator",
			path:     "/v2/acme/blobs/cache/manifests/v1.0",
			wantRepo: "acme/blobs/cache",
			wantKind: ifaces.KindManifest,
			wantRef:  "v1.0",
			wantOK:   true,
		},
		{
			name:   "not /v2/ prefix",
			path:   "/v1/library/nginx/manifests/latest",
			wantOK: false,
		},
		{
			name:   "no kind separator",
			path:   "/v2/library/nginx/latest",
			wantOK: false,
		},
		{
			name:   "path traversal repository rejected",
			path:   "/v2/../../etc/manifests/latest",
			wantOK: false,
		},
		{
			name:   "repository with dot-dot component rejected",
			path:   "/v2/foo/../bar/blobs/sha256:abc",
			wantOK: false,
		},
		{
			name:   "repository with query character rejected",
			path:   "/v2/foo?x=1/manifests/latest",
			wantOK: false,
		},
		{
			name:   "repository with fragment character rejected",
			path:   "/v2/foo#frag/blobs/sha256:abc",
			wantOK: false,
		},
		{
			name:   "uppercase repository rejected",
			path:   "/v2/Library/Nginx/manifests/latest",
			wantOK: false,
		},
		{
			name:   "empty repository component rejected",
			path:   "/v2/foo//bar/blobs/sha256:abc",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, kind, ref, ok := oci.ParseV2Path(tc.path)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v; want %v", ok, tc.wantOK)
			}

			if !ok {
				return
			}

			if repo != tc.wantRepo || kind != tc.wantKind || ref != tc.wantRef {
				t.Errorf("got (%q, %v, %q); want (%q, %v, %q)",
					repo, kind, ref, tc.wantRepo, tc.wantKind, tc.wantRef)
			}
		})
	}
}

func TestValidateRepositoryName(t *testing.T) {
	valid := []string{
		"nginx",
		"library/nginx",
		"a/b/c/d",
		"cdn/manifests-mirror/foo",
		"my_repo.v2/sub__component",
		"repo-with-dashes/x",
	}
	for _, repo := range valid {
		if err := oci.ValidateRepositoryName(repo); err != nil {
			t.Errorf("ValidateRepositoryName(%q) = %v; want nil", repo, err)
		}
	}

	invalid := []string{
		"",
		"..",
		"foo/../bar",
		"foo/./bar",
		"/leading-slash",
		"trailing-slash/",
		"double//slash",
		"UpperCase",
		"foo?x=1",
		"foo#frag",
		"foo bar",
		"foo\tbar",
		"foo\nbar",
		"foo:tag",
		"-leading-sep",
		"trailing-sep-/x",
	}
	for _, repo := range invalid {
		if err := oci.ValidateRepositoryName(repo); err == nil {
			t.Errorf("ValidateRepositoryName(%q) = nil; want error", repo)
		}
	}
}

func TestValidateRepositoryName_TooLong(t *testing.T) {
	long := strings.Repeat("a", oci.MaxRepositoryNameLength+1)
	if err := oci.ValidateRepositoryName(long); err == nil {
		t.Fatalf("ValidateRepositoryName(len=%d) = nil; want too-long error", len(long))
	}

	atLimit := strings.Repeat("a", oci.MaxRepositoryNameLength)
	if err := oci.ValidateRepositoryName(atLimit); err != nil {
		t.Fatalf("ValidateRepositoryName(len=%d) = %v; want nil", len(atLimit), err)
	}
}
