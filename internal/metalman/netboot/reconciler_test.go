// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci/oci/casext"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestOCIReconcilerMapMachineToImage(t *testing.T) {
	tests := []struct {
		name     string
		r        *OCIReconciler
		machine  *v1alpha3.Machine
		wantReqs []client.ObjectKey
	}{
		{
			name: "explicit netboot image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-explicit"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:        "ghcr.io/test/machine:v1",
					NetbootImage: "ghcr.io/test/netboot:v1",
				}},
			},
			wantReqs: []client.ObjectKey{
				{Namespace: v1alpha3.DefaultPXEArchitecture, Name: "ghcr.io/test/machine:v1"},
				{Namespace: v1alpha3.DefaultPXEArchitecture, Name: "ghcr.io/test/netboot:v1"},
			},
		},
		{
			name: "default netboot image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-default"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image: "ghcr.io/test/machine:v1",
				}},
			},
			wantReqs: []client.ObjectKey{
				{Namespace: v1alpha3.DefaultPXEArchitecture, Name: "ghcr.io/test/machine:v1"},
				{Namespace: v1alpha3.DefaultPXEArchitecture, Name: "ghcr.io/test/default-netboot:v1"},
			},
		},
		{
			name: "arm64 architecture",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-arm64"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:        "ghcr.io/test/machine:v1",
					Architecture: v1alpha3.PXEArchitectureARM64,
				}},
			},
			wantReqs: []client.ObjectKey{
				{Namespace: v1alpha3.PXEArchitectureARM64, Name: "ghcr.io/test/machine:v1"},
				{Namespace: v1alpha3.PXEArchitectureARM64, Name: "ghcr.io/test/default-netboot:v1"},
			},
		},
		{
			name: "dedupe same image",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/machine:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-dedupe"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image: "ghcr.io/test/machine:v1",
				}},
			},
			wantReqs: []client.ObjectKey{{Namespace: v1alpha3.DefaultPXEArchitecture, Name: "ghcr.io/test/machine:v1"}},
		},
		{
			name: "no pxe",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/default-netboot:v1"},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-no-pxe"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reqs := tt.r.mapMachineToImage(t.Context(), tt.machine)
			if len(reqs) != len(tt.wantReqs) {
				t.Fatalf("request count: got %d, want %d: %#v", len(reqs), len(tt.wantReqs), reqs)
			}

			for i, want := range tt.wantReqs {
				got := reqs[i].NamespacedName
				if got != want {
					t.Errorf("request %d: got %#v, want %#v", i, got, want)
				}
			}
		})
	}
}

func TestSelectPlatformDescriptor(t *testing.T) {
	amd64Path := descriptorPathForPlatform("amd64")
	arm64Path := descriptorPathForPlatform("arm64")
	singlePath := casext.DescriptorPath{Walk: []ispec.Descriptor{{Digest: "sha256:3333333333333333333333333333333333333333333333333333333333333333"}}}

	tests := []struct {
		name        string
		hostArch    string
		paths       []casext.DescriptorPath
		wantDigest  string
		wantErrPart string
	}{
		{
			name:       "selects matching platform",
			hostArch:   "arm64",
			paths:      []casext.DescriptorPath{amd64Path, arm64Path},
			wantDigest: arm64Path.Descriptor().Digest.String(),
		},
		{
			name:       "allows single manifest without platform metadata",
			hostArch:   "amd64",
			paths:      []casext.DescriptorPath{singlePath},
			wantDigest: singlePath.Descriptor().Digest.String(),
		},
		{
			name:        "errors when platform is missing",
			hostArch:    "ppc64le",
			paths:       []casext.DescriptorPath{amd64Path, arm64Path},
			wantErrPart: "no manifest found for platform linux/ppc64le",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectPlatformDescriptor(tt.hostArch, tt.paths)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErrPart)
				}

				return
			}

			if err != nil {
				t.Fatalf("selectPlatformDescriptor: %v", err)
			}

			if got.Descriptor().Digest.String() != tt.wantDigest {
				t.Fatalf("digest = %q, want %q", got.Descriptor().Digest, tt.wantDigest)
			}
		})
	}
}

func descriptorPathForPlatform(arch string) casext.DescriptorPath {
	digests := map[string]string{
		"amd64": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		"arm64": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
	}

	return casext.DescriptorPath{Walk: []ispec.Descriptor{
		{
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		{
			Digest: godigest.Digest(digests[arch]),
			Platform: &ispec.Platform{
				OS:           "linux",
				Architecture: arch,
			},
		},
	}}
}
