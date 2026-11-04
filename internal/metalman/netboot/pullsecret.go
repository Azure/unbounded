// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"oras.land/oras-go/v2/registry"
	"oras.land/oras-go/v2/registry/remote/auth"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// pullCredential reads the pull secret referenced by ref and returns the
// credential it holds for the registry of imageRef. A nil ref pulls
// anonymously.
func pullCredential(ctx context.Context, reader client.Reader, imageRef string, ref *v1alpha3.NamespacedSecretReference) (auth.Credential, error) {
	if ref == nil {
		return auth.EmptyCredential, nil
	}

	if ref.Name == "" || ref.Namespace == "" {
		return auth.EmptyCredential, fmt.Errorf("pull secret reference is incomplete")
	}

	if reader == nil {
		return auth.EmptyCredential, fmt.Errorf("pull secret %s/%s: no API reader configured", ref.Namespace, ref.Name)
	}

	parsed, err := registry.ParseReference(imageRef)
	if err != nil {
		return auth.EmptyCredential, fmt.Errorf("parsing image reference %q: %w", imageRef, err)
	}

	var secret corev1.Secret
	if err := reader.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, &secret); err != nil {
		return auth.EmptyCredential, fmt.Errorf("get pull secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	credential, err := credentialFromPullSecret(&secret, parsed.Registry)
	if err != nil {
		return auth.EmptyCredential, fmt.Errorf("pull secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}

	return credential, nil
}

type dockerConfigJSON struct {
	Auths map[string]dockerAuthConfig `json:"auths"`
}

type dockerAuthConfig struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Auth          string `json:"auth"`
	IdentityToken string `json:"identitytoken"`
	RegistryToken string `json:"registrytoken"`
}

func credentialFromPullSecret(secret *corev1.Secret, registry string) (auth.Credential, error) {
	switch secret.Type {
	case corev1.SecretTypeDockerConfigJson:
		data := secret.Data[corev1.DockerConfigJsonKey]
		if len(data) == 0 {
			return auth.EmptyCredential, fmt.Errorf("missing %s data", corev1.DockerConfigJsonKey)
		}

		var cfg dockerConfigJSON
		if err := json.Unmarshal(data, &cfg); err != nil {
			return auth.EmptyCredential, fmt.Errorf("parse %s: %w", corev1.DockerConfigJsonKey, err)
		}

		return credentialFromDockerAuths(cfg.Auths, registry)
	case corev1.SecretTypeDockercfg:
		data := secret.Data[corev1.DockerConfigKey]
		if len(data) == 0 {
			return auth.EmptyCredential, fmt.Errorf("missing %s data", corev1.DockerConfigKey)
		}

		var auths map[string]dockerAuthConfig
		if err := json.Unmarshal(data, &auths); err != nil {
			return auth.EmptyCredential, fmt.Errorf("parse %s: %w", corev1.DockerConfigKey, err)
		}

		return credentialFromDockerAuths(auths, registry)
	default:
		return auth.EmptyCredential, fmt.Errorf("unsupported secret type %q", secret.Type)
	}
}

func credentialFromDockerAuths(auths map[string]dockerAuthConfig, registry string) (auth.Credential, error) {
	if len(auths) == 0 {
		return auth.EmptyCredential, fmt.Errorf("docker config has no auth entries")
	}

	registry = normalizeRegistryHost(registry)
	for server, cfg := range auths {
		if registryHostsMatch(registry, normalizeRegistryHost(server)) {
			return credentialFromDockerAuthConfig(cfg)
		}
	}

	return auth.EmptyCredential, fmt.Errorf("docker config has no credentials for registry %q", registry)
}

func credentialFromDockerAuthConfig(cfg dockerAuthConfig) (auth.Credential, error) {
	username := cfg.Username
	password := cfg.Password

	if (username == "" || password == "") && cfg.Auth != "" {
		decoded, err := base64.StdEncoding.DecodeString(cfg.Auth)
		if err != nil {
			return auth.EmptyCredential, fmt.Errorf("decode auth field: %w", err)
		}

		user, pass, ok := strings.Cut(string(decoded), ":")
		if !ok {
			return auth.EmptyCredential, fmt.Errorf("decode auth field: missing ':' separator")
		}

		if username == "" {
			username = user
		}

		if password == "" {
			password = pass
		}
	}

	credential := auth.Credential{
		Username:     username,
		Password:     password,
		RefreshToken: cfg.IdentityToken,
		AccessToken:  cfg.RegistryToken,
	}

	if credential == auth.EmptyCredential {
		return auth.EmptyCredential, fmt.Errorf("docker auth entry has no credentials")
	}

	return credential, nil
}

func normalizeRegistryHost(server string) string {
	server = strings.TrimSpace(server)
	server = strings.TrimPrefix(server, "http://")
	server = strings.TrimPrefix(server, "https://")
	server = strings.TrimSuffix(server, "/")

	if slash := strings.Index(server, "/"); slash >= 0 {
		server = server[:slash]
	}

	return strings.ToLower(server)
}

func registryHostsMatch(registry, candidate string) bool {
	if registry == candidate {
		return true
	}

	return isDockerHubRegistry(registry) && isDockerHubRegistry(candidate)
}

func isDockerHubRegistry(registry string) bool {
	switch registry {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		return true
	default:
		return false
	}
}
