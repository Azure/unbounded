// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gomod

import "testing"

func TestVcsRef(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"v1.2.3", "v1.2.3"},
		{"v0.0.0-20240101000000-abcdef123456", "abcdef123456"},
		{"v2.0.0+incompatible", "v2.0.0"},
		{"v0.0.0-20240101000000-abcdef123456+incompatible", "abcdef123456"},
		{"v1.2.3-rc1", "v1.2.3-rc1"},
	}
	for _, tc := range cases {
		if got := vcsRef(tc.in); got != tc.want {
			t.Errorf("vcsRef(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestVanityRepoBase(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"github.com/spf13/cobra", "https://github.com/spf13/cobra"},
		{"github.com/Azure/azure-sdk-for-go/sdk/azcore", "https://github.com/Azure/azure-sdk-for-go"},
		{"golang.org/x/crypto", "https://cs.opensource.google/go/x/crypto"},
		{"k8s.io/api", "https://github.com/kubernetes/api"},
		{"k8s.io/client-go", "https://github.com/kubernetes/client-go"},
		{"sigs.k8s.io/controller-runtime", "https://github.com/kubernetes-sigs/controller-runtime"},
		{"sigs.k8s.io/yaml", "https://github.com/kubernetes-sigs/yaml"},
		{"gopkg.in/yaml.v3", "https://github.com/go-yaml/yaml"},
		{"gopkg.in/user/foo.v1", "https://github.com/user/foo"},
		{"google.golang.org/grpc", "https://github.com/grpc/grpc-go"},
		{"google.golang.org/protobuf", "https://github.com/protocolbuffers/protobuf-go"},
		{"oras.land/oras-go/v2", "https://github.com/oras-project/oras-go"},
		{"modernc.org/sqlite", "https://gitlab.com/cznic/sqlite"},
		{"golang.zx2c4.com/wireguard/wgctrl", "https://github.com/WireGuard/wgctrl-go"},
		{"example.com/foo", "https://example.com/foo"},
	}
	for _, tc := range cases {
		if got := vanityRepoBase(tc.in); got != tc.want {
			t.Errorf("vanityRepoBase(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestStripVersionSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"yaml.v3", "yaml"},
		{"foo.v10", "foo"},
		{"plain", "plain"},
		{"foo.bar", "foo.bar"},
		{"foo.vNotANumber", "foo.vNotANumber"},
	}
	for _, tc := range cases {
		if got := stripVersionSuffix(tc.in); got != tc.want {
			t.Errorf("stripVersionSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestGoRepoBase_PrefersOrigin(t *testing.T) {
	info := &goModDownload{}
	info.Origin = &struct {
		VCS string
		URL string
		Ref string
	}{URL: "https://github.com/upstream/foo.git", Ref: "refs/tags/v1.2.3"}

	gotBase, gotRef := goRepoBase("github.com/anything/else", "v0.0.0", info)
	if gotBase != "https://github.com/upstream/foo" {
		t.Errorf("base = %q", gotBase)
	}

	if gotRef != "v1.2.3" {
		t.Errorf("ref = %q", gotRef)
	}
}
