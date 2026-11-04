// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

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

func secretRef(namespace, name string) *v1alpha3.NamespacedSecretReference {
	return &v1alpha3.NamespacedSecretReference{Namespace: namespace, Name: name}
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
