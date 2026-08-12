// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package noderoute safely reconciles standalone Gantry containerd registry routes.
package noderoute

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
)

const managedMarker = "# Managed by gantryctl."

// Config is the desired set of registry-specific containerd routes on a node.
type Config struct {
	Registries []Registry `json:"registries"`
}

// Registry maps one OCI registry host to its canonical origin server.
type Registry struct {
	Host               string `json:"host"`
	Server             string `json:"server"`
	ReplaceExisting    bool   `json:"replaceExisting,omitempty"`
	ManageReplacements bool   `json:"manageReplacements,omitempty"`
}

// LoadConfig reads and validates a desired node-route configuration.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read desired registry routes: %w", err)
	}

	var config Config

	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode desired registry routes: %w", err)
	}

	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return Config{}, errors.New("decode desired registry routes: multiple JSON values")
		}

		return Config{}, fmt.Errorf("decode desired registry routes trailing data: %w", err)
	}

	if err := config.Validate(); err != nil {
		return Config{}, err
	}

	sort.Slice(config.Registries, func(i, j int) bool {
		return config.Registries[i].Host < config.Registries[j].Host
	})

	return config, nil
}

// Validate checks that every route is path-safe, canonical, and unambiguous.
func (c Config) Validate() error {
	var errs []error

	seen := make(map[string]struct{}, len(c.Registries))

	for index, registry := range c.Registries {
		host, err := NormalizeRegistryHost(registry.Host)
		if err != nil {
			errs = append(errs, fmt.Errorf("registries[%d].host: %w", index, err))
		} else if host != registry.Host {
			errs = append(errs, fmt.Errorf("registries[%d].host %q is not canonical; use %q", index, registry.Host, host))
		} else if _, ok := seen[host]; ok {
			errs = append(errs, fmt.Errorf("registries[%d].host %q is duplicated", index, host))
		} else {
			seen[host] = struct{}{}
		}

		if err := validateServer(registry.Server); err != nil {
			errs = append(errs, fmt.Errorf("registries[%d].server: %w", index, err))
		}
	}

	return errors.Join(errs...)
}

// NormalizeRegistryHost returns the canonical host[:port] used by OCI image
// references and containerd's certs.d directory layout.
func NormalizeRegistryHost(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("required")
	}

	if raw == "." || raw == ".." || strings.Contains(raw, "://") || strings.ContainsAny(raw, `/\?#@`) {
		return "", fmt.Errorf("%q must be a registry host with no scheme or path", raw)
	}

	parsed, err := url.Parse("https://" + raw)
	if err != nil || parsed.Host == "" || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid registry host %q", raw)
	}

	if parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return "", fmt.Errorf("%q must be a registry host with no path, query, or credentials", raw)
	}

	return strings.ToLower(parsed.Host), nil
}

func validateServer(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse %q: %w", raw, err)
	}

	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("%q must use http or https", raw)
	}

	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%q must contain a host and no credentials, query, or fragment", raw)
	}

	return nil
}

func renderHosts(registry Registry) []byte {
	return []byte(strings.Join([]string{
		managedMarker,
		"# Registry: " + registry.Host,
		"server = " + strconv.Quote(strings.TrimRight(registry.Server, "/")),
		"",
		`[host."http://127.0.0.1:5000"]`,
		`  capabilities = ["pull", "resolve"]`,
		`  dial_timeout = "200ms"`,
		"",
	}, "\n"))
}
