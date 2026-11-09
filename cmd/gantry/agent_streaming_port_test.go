// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/Azure/unbounded/internal/gantry/config"
	"github.com/Azure/unbounded/internal/gantry/digest"
	"github.com/Azure/unbounded/internal/gantry/streaming"
)

func TestCanonicalOriginResolverPreservesRegistryAuthority(t *testing.T) {
	d := digest.MustParse("sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	for _, test := range []struct {
		name, alias, authority string
		want                   bool
	}{
		{name: "app.azurecr.io:443", authority: "app.azurecr.io:443", want: true},
		{name: "app.azurecr.io:443", authority: "APP.AZURECR.IO:443", want: true},
		{name: "internal", alias: "app.azurecr.io:443", authority: "app.azurecr.io:443", want: true},
		{name: "app.azurecr.io:443", authority: "app.azurecr.io"},
		{name: "app.azurecr.io:443", authority: "app.azurecr.io:444"},
		{name: "app.azurecr.io", authority: "app.azurecr.io:443"},
	} {
		resolver := newCanonicalOriginResolver([]config.UpstreamRegistry{{Name: test.name, NSAlias: test.alias, Endpoint: "http://127.0.0.1:8578?ns=app.azurecr.io:443"}})

		resolved, ok := resolver(streaming.OriginURL{Raw: "https://" + test.authority + "/v2/team/app/blobs/" + d.String(), Digest: d, Canonical: true})
		if ok != test.want {
			t.Fatalf("registry %s alias %s authority %s: resolved=%v, want %v", test.name, test.alias, test.authority, ok, test.want)
		}

		if ok && resolved.Raw != "http://127.0.0.1:8578/v2/team/app/blobs/"+d.String()+"?ns=app.azurecr.io:443" {
			t.Fatalf("resolved endpoint = %q", resolved.Raw)
		}
	}
}
