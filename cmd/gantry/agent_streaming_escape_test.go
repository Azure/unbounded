// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

func TestCanonicalOriginResolverPreservesEscapedPaths(t *testing.T) {
	t.Parallel()

	for _, prefix := range []string{"/pre%2ffix", "/pre%2Ffix/", "/space%20prefix", "/percent%25prefix"} {
		t.Run(prefix, func(t *testing.T) {
			resolver := newCanonicalOriginResolver([]config.UpstreamRegistry{{
				Name: "registry", NSAlias: "app.azurecr.io", Endpoint: "http://127.0.0.1:8578" + prefix + "?ns=app.azurecr.io",
			}})
			d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
			origin := streaming.OriginURL{Raw: "https://app.azurecr.io/v2/team%2Fapp/blobs/" + d.String(), Digest: d, Canonical: true}
			resolved, ok := resolver(origin)

			if prefix[len(prefix)-1] == '/' {
				prefix = prefix[:len(prefix)-1]
			}

			want := "http://127.0.0.1:8578" + prefix + "/v2/team%2Fapp/blobs/" + d.String() + "?ns=app.azurecr.io"
			if !ok || resolved.Raw != want || resolved.Digest != d {
				t.Fatalf("resolved = %+v, ok = %v, want %q", resolved, ok, want)
			}
		})
	}
}
