// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

const (
	DefaultLocalDNSNodeListenerIP    = "169.254.10.10"
	DefaultLocalDNSClusterListenerIP = "169.254.10.11"
	DefaultLocalDNSMetricsPort       = "9253"
	DefaultLocalDNSCPUMilliCores     = 2000
	DefaultLocalDNSMemoryLimitMB     = 128
)

var localDNSPluginNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

// AgentLocalDNSConfig configures the CoreDNS cache running inside the nspawn machine.
type AgentLocalDNSConfig struct {
	Enabled              bool     `json:"Enabled"`
	NodeListenerIP       string   `json:"NodeListenerIP,omitempty"`
	ClusterListenerIP    string   `json:"ClusterListenerIP,omitempty"`
	MetricsAddress       string   `json:"MetricsAddress,omitempty"`
	CPULimitInMilliCores *int     `json:"CPULimitInMilliCores,omitempty"`
	MemoryLimitInMB      *int     `json:"MemoryLimitInMB,omitempty"`
	RequiredPlugins      []string `json:"RequiredPlugins,omitempty"`
	CorefileTemplate     string   `json:"CorefileTemplate,omitempty"`
}

// DeepCopy returns a copy with independently owned mutable fields.
func (c *AgentLocalDNSConfig) DeepCopy() *AgentLocalDNSConfig {
	if c == nil {
		return nil
	}

	out := *c
	out.RequiredPlugins = append([]string(nil), c.RequiredPlugins...)

	if c.CPULimitInMilliCores != nil {
		value := *c.CPULimitInMilliCores
		out.CPULimitInMilliCores = &value
	}

	if c.MemoryLimitInMB != nil {
		value := *c.MemoryLimitInMB
		out.MemoryLimitInMB = &value
	}

	return &out
}

// validateLocalDNS checks LocalDNS fields that do not require host discovery.
// Keeping this on AgentConfig ensures cross-field validation has access to the
// complete configuration as LocalDNS gains additional integration points.
func (a *AgentConfig) validateLocalDNS() error {
	c := a.LocalDNS
	if c == nil || !c.Enabled {
		return nil
	}

	var errs []error

	nodeListener, err := parseLocalDNSIPv4(defaultString(c.NodeListenerIP, DefaultLocalDNSNodeListenerIP))
	if err != nil {
		errs = append(errs, fmt.Errorf("LocalDNS.NodeListenerIP: %w", err))
	}

	clusterListener, err := parseLocalDNSIPv4(defaultString(c.ClusterListenerIP, DefaultLocalDNSClusterListenerIP))
	if err != nil {
		errs = append(errs, fmt.Errorf("LocalDNS.ClusterListenerIP: %w", err))
	}

	clusterDNSAddr, err := parseLocalDNSIPv4(a.Cluster.ClusterDNS)
	if err != nil {
		errs = append(errs, fmt.Errorf("Cluster.ClusterDNS for LocalDNS: %w", err))
	}

	if nodeListener.IsValid() && clusterListener.IsValid() && nodeListener == clusterListener {
		errs = append(errs, fmt.Errorf("LocalDNS listener IPs must be distinct"))
	}

	if clusterDNSAddr.IsValid() && (clusterDNSAddr == nodeListener || clusterDNSAddr == clusterListener) {
		errs = append(errs, fmt.Errorf("Cluster.ClusterDNS must be distinct from LocalDNS listeners"))
	}

	if strings.TrimSpace(c.MetricsAddress) != "" {
		if err := validateLocalDNSMetricsAddress(c.MetricsAddress); err != nil {
			errs = append(errs, fmt.Errorf("LocalDNS.MetricsAddress: %w", err))
		}
	}

	if c.CPULimitInMilliCores != nil && *c.CPULimitInMilliCores <= 0 {
		errs = append(errs, fmt.Errorf("LocalDNS.CPULimitInMilliCores must be positive"))
	}

	if c.MemoryLimitInMB != nil && *c.MemoryLimitInMB <= 0 {
		errs = append(errs, fmt.Errorf("LocalDNS.MemoryLimitInMB must be positive"))
	}

	seenPlugins := map[string]struct{}{}

	for _, raw := range c.RequiredPlugins {
		plugin := strings.TrimSpace(strings.ToLower(raw))
		if !localDNSPluginNamePattern.MatchString(plugin) {
			errs = append(errs, fmt.Errorf("LocalDNS.RequiredPlugins entry %q is invalid", raw))
			continue
		}

		if _, ok := seenPlugins[plugin]; ok {
			errs = append(errs, fmt.Errorf("LocalDNS.RequiredPlugins entry %q is duplicated", raw))
		}

		seenPlugins[plugin] = struct{}{}
	}

	if c.CorefileTemplate != "" {
		// This is an Unbounded input-safety limit rather than an AgentBaker
		// compatibility requirement. It bounds template parsing and the config
		// carried through goal state while leaving ample room for custom policy.
		if len(c.CorefileTemplate) > 256*1024 {
			errs = append(errs, fmt.Errorf("LocalDNS.CorefileTemplate exceeds 256 KiB"))
		} else if _, err := template.New("localdns-corefile").Option("missingkey=error").Parse(c.CorefileTemplate); err != nil {
			errs = append(errs, fmt.Errorf("parse LocalDNS.CorefileTemplate: %w", err))
		}
	}

	return errors.Join(errs...)
}

func parseLocalDNSIPv4(value string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return netip.Addr{}, fmt.Errorf("must be an IPv4 address: %w", err)
	}

	if !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("must be a unicast IPv4 address")
	}

	return addr, nil
}

func validateLocalDNSMetricsAddress(configured string) error {
	host, port, err := net.SplitHostPort(strings.TrimSpace(configured))
	if err != nil {
		return fmt.Errorf("must use IP:port syntax: %w", err)
	}

	if _, err := parseLocalDNSIPv4(host); err != nil {
		return err
	}

	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}

	return strings.TrimSpace(value)
}
