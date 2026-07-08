// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"encoding/base64"
	"strings"
	"testing"

	godigest "github.com/opencontainers/go-digest"
	ispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/opencontainers/umoci/oci/casext"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestOCIReconcilerMapMachineToImage(t *testing.T) {
	tests := []struct {
		name     string
		r        *OCIReconciler
		machine  *v1alpha3.Machine
		wantReqs []imagePullRequest
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
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1"},
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/netboot:v1"},
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
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1"},
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/default-netboot:v1"},
			},
		},
		{
			name: "pull secrets",
			r: &OCIReconciler{
				DefaultNetbootRef:           "ghcr.io/test/default-netboot:v1",
				DefaultNetbootPullSecretRef: secretRef("unbounded-kube", "default-netboot"),
			},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-secrets"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:         "ghcr.io/test/machine:v1",
					PullSecretRef: secretRef("tenant-a", "machine-image"),
				}},
			},
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1", PullSecretRef: secretRef("tenant-a", "machine-image")},
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/default-netboot:v1", PullSecretRef: secretRef("unbounded-kube", "default-netboot")},
			},
		},
		{
			name: "explicit netboot pull secret",
			r: &OCIReconciler{
				DefaultNetbootRef:           "ghcr.io/test/default-netboot:v1",
				DefaultNetbootPullSecretRef: secretRef("unbounded-kube", "default-netboot"),
			},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-explicit-netboot-secret"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:                "ghcr.io/test/machine:v1",
					NetbootImage:         "ghcr.io/test/netboot:v1",
					NetbootPullSecretRef: secretRef("tenant-a", "netboot-image"),
				}},
			},
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1"},
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/netboot:v1", PullSecretRef: secretRef("tenant-a", "netboot-image")},
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
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.PXEArchitectureARM64, ImageRef: "ghcr.io/test/machine:v1"},
				{Architecture: v1alpha3.PXEArchitectureARM64, ImageRef: "ghcr.io/test/default-netboot:v1"},
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
			wantReqs: []imagePullRequest{{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1"}},
		},
		{
			name: "does not dedupe different pull secrets",
			r:    &OCIReconciler{DefaultNetbootRef: "ghcr.io/test/machine:v1", DefaultNetbootPullSecretRef: secretRef("tenant-a", "netboot")},
			machine: &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "node-different-secrets"},
				Spec: v1alpha3.MachineSpec{PXE: &v1alpha3.PXESpec{
					Image:         "ghcr.io/test/machine:v1",
					PullSecretRef: secretRef("tenant-a", "machine"),
				}},
			},
			wantReqs: []imagePullRequest{
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1", PullSecretRef: secretRef("tenant-a", "machine")},
				{Architecture: v1alpha3.DefaultPXEArchitecture, ImageRef: "ghcr.io/test/machine:v1", PullSecretRef: secretRef("tenant-a", "netboot")},
			},
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
				got, err := decodeImagePullRequest(reqs[i].NamespacedName)
				if err != nil {
					t.Fatalf("decode request %d: %v", i, err)
				}

				if got.ImageRef != want.ImageRef || got.Architecture != want.Architecture || !secretRefsEqual(got.PullSecretRef, want.PullSecretRef) {
					t.Errorf("request %d: got %#v, want %#v", i, got, want)
				}
			}
		})
	}
}

func TestCredentialFromPullSecret(t *testing.T) {
	tests := []struct {
		name        string
		secret      *corev1.Secret
		registry    string
		wantUser    string
		wantPass    string
		wantRefresh string
		wantAccess  string
		wantErrPart string
	}{
		{
			name:     "docker config json username password",
			secret:   dockerConfigJSONSecret("pull", `{"auths":{"ghcr.io":{"username":"user","password":"pass"}}}`),
			registry: "ghcr.io",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:     "docker config json auth field",
			secret:   dockerConfigJSONSecret("pull", `{"auths":{"https://ghcr.io/v1/":{"auth":"`+base64.StdEncoding.EncodeToString([]byte("user:pass"))+`"}}}`),
			registry: "ghcr.io",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:        "identity and registry tokens",
			secret:      dockerConfigJSONSecret("pull", `{"auths":{"ghcr.io":{"identitytoken":"refresh","registrytoken":"access"}}}`),
			registry:    "ghcr.io",
			wantRefresh: "refresh",
			wantAccess:  "access",
		},
		{
			name:     "dockercfg",
			secret:   dockerCfgSecret("pull", `{"ghcr.io":{"username":"user","password":"pass"}}`),
			registry: "ghcr.io",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:     "docker hub aliases",
			secret:   dockerConfigJSONSecret("pull", `{"auths":{"https://index.docker.io/v1/":{"username":"user","password":"pass"}}}`),
			registry: "registry-1.docker.io",
			wantUser: "user",
			wantPass: "pass",
		},
		{
			name:        "missing registry",
			secret:      dockerConfigJSONSecret("pull", `{"auths":{"ghcr.io":{"username":"user","password":"pass"}}}`),
			registry:    "example.com",
			wantErrPart: "no credentials for registry",
		},
		{
			name:        "malformed data",
			secret:      dockerConfigJSONSecret("pull", `{`),
			registry:    "ghcr.io",
			wantErrPart: "parse .dockerconfigjson",
		},
		{
			name:        "bad auth field",
			secret:      dockerConfigJSONSecret("pull", `{"auths":{"ghcr.io":{"auth":"not-base64"}}}`),
			registry:    "ghcr.io",
			wantErrPart: "decode auth field",
		},
		{
			name: "unsupported type",
			secret: &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "pull"},
				Type:       corev1.SecretTypeOpaque,
			},
			registry:    "ghcr.io",
			wantErrPart: "unsupported secret type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := credentialFromPullSecret(tt.secret, tt.registry)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErrPart)
				}

				return
			}

			if err != nil {
				t.Fatalf("credentialFromPullSecret: %v", err)
			}

			if got.Username != tt.wantUser || got.Password != tt.wantPass || got.RefreshToken != tt.wantRefresh || got.AccessToken != tt.wantAccess {
				t.Fatalf("credential = %#v", got)
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

func secretRef(namespace, name string) *v1alpha3.NamespacedSecretReference {
	return &v1alpha3.NamespacedSecretReference{Namespace: namespace, Name: name}
}

func secretRefsEqual(a, b *v1alpha3.NamespacedSecretReference) bool {
	if a == nil || b == nil {
		return a == b
	}

	return a.Namespace == b.Namespace && a.Name == b.Name
}

func dockerConfigJSONSecret(name, data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(data)},
	}
}

func dockerCfgSecret(name, data string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Type:       corev1.SecretTypeDockercfg,
		Data:       map[string][]byte{corev1.DockerConfigKey: []byte(data)},
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
