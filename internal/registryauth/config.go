// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package registryauth implements operator-managed kubelet authentication for ACR.
package registryauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/gantry/oci"
)

const (
	// ConfigKey is the optional installation policy in gantry-config.
	ConfigKey = "node-auth.yaml"
	// ProviderName is the executable name kubelet invokes.
	ProviderName = "unbounded-acr-credential-provider"
	// FlexNodeLabel and FlexNodeValue identify AKS Flex nodes, not native AKS nodes.
	FlexNodeLabel = "kubernetes.azure.com/nodepool-type"
	FlexNodeValue = "FlexNodes"
)

var (
	acrHost  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)?(?:\.[a-z0-9]+\.geo)?\.azurecr\.io$`)
	imageTag = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// Config is the public installation policy plus the operator-derived registries.
// Registries are never read from node-auth.yaml; gantry-config is authoritative.
type Config struct {
	Mode       string   `json:"mode,omitempty"`
	ClientID   string   `json:"clientID,omitempty"`
	ProbeImage string   `json:"probeImage,omitempty"`
	Registries []string `json:"registries,omitempty"`
}

// IsACR reports whether host is a public-cloud ACR login server.
func IsACR(host string) bool { return len(host) <= 253 && acrHost.MatchString(host) }

// Derive reads node-auth policy and derives eligible upstreams without modifying
// the administrator's Gantry payload. Explicit shared credentials bypass this path.
func Derive(gantry, policy string) (Config, error) {
	var cfg Config
	if policy != "" {
		if err := yaml.UnmarshalStrict([]byte(policy), &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", ConfigKey, err)
		}

		if len(cfg.Registries) != 0 {
			return Config{}, errors.New("node-auth registries must come from Gantry upstream_registries")
		}
	}

	if cfg.Mode == "" {
		cfg.Mode = "auto"
	}

	if cfg.Mode != "auto" && cfg.Mode != "external" {
		return Config{}, errors.New("node-auth mode must be auto or external")
	}

	if cfg.ClientID != "" {
		if _, err := uuid.Parse(cfg.ClientID); err != nil {
			return Config{}, errors.New("node-auth clientID must be a managed identity client UUID")
		}
	}

	if cfg.Mode == "external" {
		if cfg.ProbeImage != "" {
			return Config{}, errors.New("node-auth probeImage requires auto mode")
		}

		return cfg, nil
	}

	var upstreams struct {
		Registries []struct {
			Name            string `json:"name"`
			Endpoint        string `json:"endpoint"`
			CredentialsPath string `json:"credentials_path"`
		} `json:"upstream_registries"`
	}
	if err := yaml.Unmarshal([]byte(gantry), &upstreams); err != nil {
		return Config{}, fmt.Errorf("parse Gantry upstreams: %w", err)
	}

	for _, upstream := range upstreams.Registries {
		if !IsACR(upstream.Name) || upstream.CredentialsPath != "" {
			continue
		}

		u, err := url.Parse(upstream.Endpoint)
		if err != nil || u.Scheme != "https" || u.Host != upstream.Name || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return Config{}, fmt.Errorf("ACR upstream %s must use its matching HTTPS endpoint; use external node-auth mode for custom authentication", upstream.Name)
		}

		cfg.Registries = append(cfg.Registries, upstream.Name)
	}

	slices.Sort(cfg.Registries)

	cfg.Registries = slices.Compact(cfg.Registries)
	if cfg.ProbeImage != "" {
		host, _, _, err := ParseImage(cfg.ProbeImage)
		if err != nil || !slices.Contains(cfg.Registries, host) {
			return Config{}, errors.New("node-auth probeImage must be an image in a configured managed-identity ACR upstream")
		}
	}

	return cfg, nil
}

// ParseImage separates an explicit ACR image into registry, repository, and reference.
func ParseImage(image string) (string, string, string, error) {
	host, rest, ok := strings.Cut(image, "/")
	if !ok || !IsACR(host) || strings.ContainsAny(rest, " \t\r\n?#%\\") {
		return "", "", "", errors.New("expected an explicit public-cloud ACR image reference")
	}

	repo, ref, hasDigest := strings.Cut(rest, "@")
	if hasDigest {
		if _, err := digest.Parse(ref); err != nil {
			return "", "", "", errors.New("invalid ACR image digest")
		}

		if pos := strings.LastIndex(repo, ":"); pos >= 0 {
			if !imageTag.MatchString(repo[pos+1:]) {
				return "", "", "", errors.New("invalid ACR image tag")
			}

			repo = repo[:pos]
		}
	} else {
		repo, ref = rest, "latest"
		if pos := strings.LastIndex(rest, ":"); pos >= 0 {
			repo, ref = rest[:pos], rest[pos+1:]
		}

		if !imageTag.MatchString(ref) {
			return "", "", "", errors.New("invalid ACR image tag")
		}
	}

	if oci.ValidateRepositoryName(repo) != nil {
		return "", "", "", errors.New("invalid ACR image repository")
	}

	return host, repo, ref, nil
}

// ProviderConfig renders kubelet's stable v1 exec provider configuration.
func (c Config) ProviderConfig() ([]byte, error) {
	args := []string{"acr-credential-provider"}
	if c.ClientID != "" {
		args = append(args, "--client-id="+c.ClientID)
	}

	for _, registry := range c.Registries {
		args = append(args, "--registry="+registry)
	}

	return json.MarshalIndent(map[string]any{
		"apiVersion": "kubelet.config.k8s.io/v1", "kind": "CredentialProviderConfig",
		"providers": []any{map[string]any{
			"name": ProviderName, "matchImages": c.Registries, "defaultCacheDuration": "5m",
			"apiVersion": "credentialprovider.kubelet.k8s.io/v1", "args": args,
		}},
	}, "", "  ")
}
