// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"fmt"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
)

type nodeSpec struct {
	ID          uint64
	SSHTarget   string
	FabricAddr  string
	ListenAddr  string
	MetricsAddr string
	ConfigPath  string
	Workdir     string
	DiskPath    string
	BlockDevice string
	ForwardURL  string
}

func parseNodeSpecs(opts options) ([]nodeSpec, error) {
	nodes := make([]nodeSpec, 0, len(opts.nodeSpecs))
	ids := map[uint64]bool{}

	for _, spec := range opts.nodeSpecs {
		node, err := parseNodeSpec(spec, opts)
		if err != nil {
			return nil, err
		}

		if ids[node.ID] {
			return nil, fmt.Errorf("duplicate node id %d", node.ID)
		}

		ids[node.ID] = true
		nodes = append(nodes, node)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })

	return nodes, nil
}

func parseNodeSpec(spec string, opts options) (nodeSpec, error) {
	fields, err := parseKeyValues(spec)
	if err != nil {
		return nodeSpec{}, err
	}

	idText := firstNonEmpty(fields["id"], fields["node"])
	if idText == "" {
		return nodeSpec{}, fmt.Errorf("node spec %q missing id", spec)
	}

	id, err := strconv.ParseUint(idText, 10, 64)
	if err != nil || id == 0 {
		return nodeSpec{}, fmt.Errorf("node spec %q has invalid id %q", spec, idText)
	}

	node := nodeSpec{
		ID:          id,
		SSHTarget:   firstNonEmpty(fields["ssh"], fields["host"]),
		FabricAddr:  fields["fabric"],
		ListenAddr:  fields["listen"],
		MetricsAddr: fields["metrics"],
		ConfigPath:  firstNonEmpty(fields["config"], opts.remoteConfig),
		Workdir:     firstNonEmpty(fields["workdir"], opts.remoteWorkdir),
		DiskPath:    fields["disk"],
		BlockDevice: firstNonEmpty(fields["block"], fields["block_device"]),
	}

	if node.SSHTarget == "" {
		return nodeSpec{}, fmt.Errorf("node %d missing ssh target", id)
	}

	if node.FabricAddr == "" {
		return nodeSpec{}, fmt.Errorf("node %d missing fabric address", id)
	}
	if node.ListenAddr == "" {
		node.ListenAddr = node.FabricAddr
	}

	if node.MetricsAddr == "" {
		return nodeSpec{}, fmt.Errorf("node %d missing metrics address", id)
	}

	if opts.sshUser != "" && !strings.Contains(node.SSHTarget, "@") {
		node.SSHTarget = opts.sshUser + "@" + node.SSHTarget
	}

	if node.Workdir == "" {
		node.Workdir = "/tmp"
	}

	if node.ConfigPath == "" {
		node.ConfigPath = path.Join(node.Workdir, fmt.Sprintf("unbounded-storage-bench-node-%d.toml", node.ID))
	}

	if node.DiskPath == "" && node.BlockDevice == "" {
		node.DiskPath = path.Join(node.Workdir, fmt.Sprintf("unbounded-storage-bench-node-%d.disk", node.ID))
	}

	if _, _, err := net.SplitHostPort(node.MetricsAddr); err != nil {
		return nodeSpec{}, fmt.Errorf("node %d metrics address %q must be host:port: %w", id, node.MetricsAddr, err)
	}

	return node, nil
}

func parseKeyValues(spec string) (map[string]string, error) {
	fields := map[string]string{}

	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("node spec part %q must be key=value", part)
		}

		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" || value == "" {
			return nil, fmt.Errorf("node spec part %q must have non-empty key and value", part)
		}

		fields[key] = value
	}

	return fields, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
