// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cloudinit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestValidateUserData(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		content string
		wantErr string
	}{
		{
			name: "mapping without users",
			content: `#cloud-config
packages:
- jq
write_files:
- path: /etc/example
  content: "users: is data here"
`,
		},
		{
			name: "reject users",
			content: `#cloud-config
users:
- name: temporary-admin
`,
			wantErr: "must not define top-level users",
		},
		{
			name: "reject Jinja users bypass",
			content: `## template: jinja
#cloud-config
"{{ 'users' }}":
- name: temporary-admin
`,
			wantErr: "cloud-init templating is not allowed",
		},
		{name: "reject empty", wantErr: "exactly one YAML mapping document"},
		{name: "reject invalid yaml", content: "users: [", wantErr: "parse cloud-init user-data"},
		{name: "reject sequence", content: "- package\n", wantErr: "exactly one YAML mapping document"},
		{
			name: "reject second document",
			content: `#cloud-config
packages: [jq]
---
runcmd: [echo unexpected]
`,
			wantErr: "exactly one YAML document",
		},
		{
			name: "reject trailing scalar document",
			content: `#cloud-config
packages: [jq]
--- trailing
`,
			wantErr: "exactly one YAML document",
		},
		{
			name: "reject duplicate key",
			content: `#cloud-config
packages: [jq]
packages: [curl]
`,
			wantErr: "already defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateUserData(strings.NewReader(tt.content))
			if tt.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}

func TestResolveUserDataScopesValidationToHillsboroImage(t *testing.T) {
	t.Parallel()

	for _, source := range []string{"data", "binaryData"} {
		t.Run(source, func(t *testing.T) {
			t.Parallel()

			scheme := runtime.NewScheme()
			require.NoError(t, corev1.AddToScheme(scheme))

			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "default"},
			}
			userData := []byte("#cloud-config\nusers: []\n")
			if source == "data" {
				configMap.Data = map[string]string{"user-data": string(userData)}
			} else {
				configMap.BinaryData = map[string][]byte{"user-data": userData}
			}

			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
			machine := machineWithUserData("ghcr.io/azure/host-ubuntu2404-gb200-hillsboro@sha256:abc")

			_, _, err := ResolveUserData(t.Context(), reader, machine)
			require.ErrorContains(t, err, "must not define top-level users")
			require.True(t, IsPermanentError(err))

			machine.Spec.Host = &v1alpha3.HostSpec{Netboot: machine.Spec.PXE.DeepCopy()}
			machine.Spec.PXE = nil

			_, _, err = ResolveUserData(t.Context(), reader, machine)
			require.ErrorContains(t, err, "must not define top-level users")

			machine.Spec.Host.Netboot.Image = "ghcr.io/azure/host-ubuntu2404:v1"
			resolved, ok, err := ResolveUserData(t.Context(), reader, machine)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, userData, resolved)
		})
	}
}

func TestResolveUserDataClassifiesMissingKeyAsPermanent(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "default"},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	machine := machineWithUserData("ghcr.io/azure/host-ubuntu2404-gb200-hillsboro:v1")

	_, _, err := ResolveUserData(t.Context(), reader, machine)

	require.ErrorContains(t, err, `key "user-data" not found`)
	require.True(t, IsPermanentError(err))
}

func TestResolveUserDataAllowsTemplatingForGenericImages(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	userData := []byte(`## template: jinja
#cloud-config
"{{ 'users' }}":
- name: temporary-admin
`)
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "access", Namespace: "default"},
		Data:       map[string]string{"user-data": string(userData)},
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configMap).Build()
	machine := machineWithUserData("ghcr.io/azure/host-ubuntu2404:v1")

	resolved, ok, err := ResolveUserData(t.Context(), reader, machine)

	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, userData, resolved)
}

func TestPreservesImageUsers(t *testing.T) {
	t.Parallel()

	for _, image := range []string{
		"ghcr.io/azure/host-ubuntu2404-gb200-hillsboro",
		"ghcr.io/azure/host-ubuntu2404-gb200-hillsboro:test",
		"ghcr.io/azure/host-ubuntu2404-gb200-hillsboro@sha256:abc",
		"GHCR.IO/AZURE/HOST-UBUNTU2404-GB200-HILLSBORO:TEST",
	} {
		require.True(t, PreservesImageUsers(image), image)
	}
	for _, image := range []string{
		"ghcr.io/azure/host-ubuntu2404-gb200",
		"ghcr.io/azure/host-ubuntu2404-gb200-hillsboro-copy:test",
		"example.com/azure/host-ubuntu2404-gb200-hillsboro:test",
	} {
		require.False(t, PreservesImageUsers(image), image)
	}
}

func machineWithUserData(image string) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: image,
				CloudInit: &v1alpha3.CloudInitSpec{
					UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{
						Name:      "access",
						Namespace: "default",
					},
				},
			},
		},
	}
}
