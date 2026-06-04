// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package oci_test

import (
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
			name:   "not /v2/ prefix",
			path:   "/v1/library/nginx/manifests/latest",
			wantOK: false,
		},
		{
			name:   "no kind separator",
			path:   "/v2/library/nginx/latest",
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
