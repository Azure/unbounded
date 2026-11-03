// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package registryauth installs the stock ACR credential provider when explicitly requested.
package registryauth

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"sigs.k8s.io/yaml"
)

const (
	ConfigKey           = "node-auth.yaml"
	ProviderName        = "acr-credential-provider"
	ProviderBinary      = "/unbounded/bin/acr-credential-provider"
	ExternalMode        = "external"
	IdentityBindingMode = "aks-identity-binding"
	FlexNodeLabel       = "kubernetes.azure.com/nodepool-type"
	FlexNodeValue       = "FlexNodes"
	IdentityRoleName    = "gantry-acr-pull"
)

var acrHost = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)?(?:\.[a-z0-9]+\.geo)?\.azurecr\.io$`)

// Config contains installation policy, never access tokens or client secrets.
// An absent policy leaves customer authentication entirely alone.
type Config struct {
	Mode         string `json:"mode,omitempty"`
	Registry     string `json:"registry,omitempty"`
	ClientID     string `json:"clientID,omitempty"`
	TenantID     string `json:"tenantID,omitempty"`
	SNIName      string `json:"sniName,omitempty"`
	ProxyAddress string `json:"proxyAddress,omitempty"`
	Audience     string `json:"audience,omitempty"`
	CACert       string `json:"caCert,omitempty"`
}

func IsACR(host string) bool { return len(host) <= 253 && acrHost.MatchString(host) }

// Derive validates the explicitly selected policy against Gantry's actual upstream.
func Derive(gantry, policy string) (Config, error) {
	cfg := Config{Mode: ExternalMode}
	if policy != "" {
		if err := yaml.UnmarshalStrict([]byte(policy), &cfg); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", ConfigKey, err)
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}

	if cfg.Mode == ExternalMode {
		return cfg, nil
	}

	var upstreams struct {
		Registries []struct {
			Name            string `json:"name"`
			Endpoint        string `json:"endpoint"`
			CredentialsPath string `json:"credentials_path"`
		} `json:"upstream_registries"`
	}

	data, err := yaml.YAMLToJSONStrict([]byte(gantry))
	if err != nil {
		return Config{}, fmt.Errorf("parse Gantry upstreams: %w", err)
	}

	if err := json.Unmarshal(data, &upstreams); err != nil {
		return Config{}, fmt.Errorf("parse Gantry upstreams: %w", err)
	}

	for _, upstream := range upstreams.Registries {
		if upstream.Name != cfg.Registry {
			continue
		}

		if upstream.CredentialsPath != "" {
			return Config{}, errors.New("managed ACR authentication conflicts with Gantry credentials_path; use external authentication")
		}

		if !matchingRegistryEndpoint(upstream.Endpoint, cfg.Registry) {
			return Config{}, errors.New("managed ACR authentication requires the registry's matching HTTPS endpoint")
		}

		return cfg, nil
	}

	return Config{}, errors.New("managed ACR authentication requires its registry in Gantry upstream_registries")
}

func matchingRegistryEndpoint(endpoint, registry string) bool {
	u, err := url.Parse(endpoint)

	return err == nil && u.Scheme == "https" && u.Host == registry && u.User == nil &&
		(u.Path == "" || u.Path == "/") && u.RawQuery == "" && u.Fragment == ""
}

func (c Config) Validate() error {
	if c.Mode != ExternalMode && c.Mode != IdentityBindingMode {
		return errors.New("node-auth mode must be external or aks-identity-binding")
	}

	if c.Registry != "" && !IsACR(c.Registry) {
		return errors.New("node-auth registry must be a public-cloud ACR login server")
	}

	if c.Mode == ExternalMode {
		return nil
	}

	if c.Registry == "" {
		return errors.New("aks-identity-binding requires an explicit registry")
	}

	if id, err := uuid.Parse(c.ClientID); err != nil || id == uuid.Nil {
		return errors.New("aks-identity-binding requires a nonzero clientID UUID")
	}

	if id, err := uuid.Parse(c.TenantID); err != nil || id == uuid.Nil {
		return errors.New("aks-identity-binding requires a nonzero tenantID UUID")
	}

	if !validHostname(c.SNIName) {
		return errors.New("aks-identity-binding requires the AKS token proxy SNI hostname")
	}

	if net.ParseIP(c.ProxyAddress) == nil && !validHostname(c.ProxyAddress) {
		return errors.New("aks-identity-binding requires the AKS token proxy IP address or hostname")
	}

	if c.Audience == "" || strings.ContainsAny(c.Audience, " \t\r\n") {
		return errors.New("aks-identity-binding requires the discovered ServiceAccount token audience")
	}

	if !x509.NewCertPool().AppendCertsFromPEM([]byte(c.CACert)) {
		return errors.New("aks-identity-binding requires the AKS token proxy CA certificate")
	}

	return nil
}

func validHostname(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}

	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}

		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}

	return true
}

// ProviderConfig configures upstream's identity-binding mode, not its IMDS mode.
func (c Config) ProviderConfig() ([]byte, error) {
	return json.MarshalIndent(map[string]any{
		"apiVersion": "kubelet.config.k8s.io/v1", "kind": "CredentialProviderConfig",
		"providers": []any{map[string]any{
			"name": ProviderName, "matchImages": []string{c.Registry}, "defaultCacheDuration": "5m",
			"apiVersion": "credentialprovider.kubelet.k8s.io/v1",
			"args": []string{
				azureConfigPath, "--ib-sni-name=" + c.SNIName,
				"--ib-default-client-id=" + c.ClientID, "--ib-default-tenant-id=" + c.TenantID,
				"--ib-apiserver-ip=" + c.ProxyAddress,
			},
			"tokenAttributes": map[string]any{
				"serviceAccountTokenAudience": c.Audience, "cacheType": "Token", "requireServiceAccount": true,
			},
		}},
	}, "", "  ")
}

func (c Config) AzureConfig() ([]byte, error) {
	return json.Marshal(map[string]any{"cloud": "AzurePublicCloud", "tenantId": c.TenantID, "useManagedIdentityExtension": true})
}
