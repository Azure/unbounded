// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const (
	maxNodeExporterExtraArgs     = 128
	maxNodeExporterExtraArgsSize = 64 * 1024
)

// AgentNodeExporterConfig configures Prometheus node exporter inside the nspawn machine.
type AgentNodeExporterConfig struct {
	Enabled       bool                   `json:"Enabled"`
	ListenAddress string                 `json:"ListenAddress,omitempty"`
	ExtraArgs     []string               `json:"ExtraArgs,omitempty"`
	TLS           *NodeExporterTLSConfig `json:"TLS,omitempty"`
}

// NodeExporterTLSConfig configures native node exporter TLS with user-managed files.
type NodeExporterTLSConfig struct {
	Enabled         bool   `json:"Enabled"`
	CertificateFile string `json:"CertificateFile,omitempty"`
	PrivateKeyFile  string `json:"PrivateKeyFile,omitempty"`
	ClientCAFile    string `json:"ClientCAFile,omitempty"`
}

// DeepCopy returns a copy with independently owned mutable fields.
func (c *AgentNodeExporterConfig) DeepCopy() *AgentNodeExporterConfig {
	if c == nil {
		return nil
	}

	out := *c

	out.ExtraArgs = append([]string(nil), c.ExtraArgs...)
	if c.TLS != nil {
		tls := *c.TLS
		out.TLS = &tls
	}

	return &out
}

func (a *AgentConfig) validateNodeExporter() error {
	c := a.NodeExporter
	if c == nil || !c.Enabled {
		return nil
	}

	var errs []error

	if strings.TrimSpace(c.ListenAddress) != "" {
		if err := validateNodeExporterListenAddress(c.ListenAddress); err != nil {
			errs = append(errs, fmt.Errorf("NodeExporter.ListenAddress: %w", err))
		}
	}

	if len(c.ExtraArgs) > maxNodeExporterExtraArgs {
		errs = append(errs, fmt.Errorf("NodeExporter.ExtraArgs cannot contain more than %d entries", maxNodeExporterExtraArgs))
	}

	totalSize := 0
	for _, arg := range c.ExtraArgs {
		totalSize += len(arg)
		if strings.TrimSpace(arg) == "" {
			errs = append(errs, fmt.Errorf("NodeExporter.ExtraArgs cannot contain an empty argument"))
			continue
		}

		if strings.ContainsFunc(arg, unicode.IsControl) {
			errs = append(errs, fmt.Errorf("NodeExporter.ExtraArgs argument contains a control character"))
		}

		name := arg
		if before, _, ok := strings.Cut(arg, "="); ok {
			name = before
		}

		if name == "--web.listen-address" || name == "--web.config.file" {
			errs = append(errs, fmt.Errorf("NodeExporter.ExtraArgs cannot override %s", name))
		}
	}

	if totalSize > maxNodeExporterExtraArgsSize {
		errs = append(errs, fmt.Errorf("NodeExporter.ExtraArgs exceeds %d bytes", maxNodeExporterExtraArgsSize))
	}

	if err := validateNodeExporterTLS(c.TLS); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

func validateNodeExporterListenAddress(value string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(value))
	if err != nil {
		return fmt.Errorf("must use IP:port syntax: %w", err)
	}

	addr, err := netip.ParseAddr(host)
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return fmt.Errorf("host must be a unicast IPv4 address")
	}

	parsedPort, err := strconv.Atoi(port)
	if err != nil || parsedPort < 1 || parsedPort > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}

func validateNodeExporterTLS(tls *NodeExporterTLSConfig) error {
	if tls == nil || !tls.Enabled {
		return nil
	}

	var errs []error

	for name, path := range map[string]string{
		"CertificateFile": tls.CertificateFile,
		"PrivateKeyFile":  tls.PrivateKeyFile,
	} {
		if err := validateNodeExporterPath(path); err != nil {
			errs = append(errs, fmt.Errorf("NodeExporter.TLS.%s: %w", name, err))
		}
	}

	if tls.ClientCAFile != "" {
		if err := validateNodeExporterPath(tls.ClientCAFile); err != nil {
			errs = append(errs, fmt.Errorf("NodeExporter.TLS.ClientCAFile: %w", err))
		}
	}

	return errors.Join(errs...)
}

func validateNodeExporterPath(path string) error {
	if path == "" {
		return fmt.Errorf("is required")
	}

	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("must be a clean absolute path")
	}

	if strings.ContainsFunc(path, unicode.IsSpace) || strings.ContainsFunc(path, unicode.IsControl) {
		return fmt.Errorf("must not contain whitespace or control characters")
	}

	return nil
}
