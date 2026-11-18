// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package identity works out a node's Nebraska identity the way
// trident-acl-agent does, and reads and writes the agent-info annotation that
// carries it to centerpivot.
package identity

import (
	"crypto/sha512"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Masterminds/semver/v3"
	"github.com/google/uuid"
)

// AgentInfoSchemaVersion is agent-info's schema version.
const AgentInfoSchemaVersion = "1.0"

// InstanceID returns the instance ID trident-acl-agent sends Nebraska for a
// host whose /etc/machine-id holds machineID: the first 16 bytes of the
// SHA-384 hash of the machine ID's 16 bytes, as a UUID (MachineId::hashed_uuid
// in Trident's osutils/src/machine_id.rs).
func InstanceID(machineID string) (string, error) {
	id, err := uuid.Parse(strings.TrimSpace(machineID))
	if err != nil || len(strings.TrimSpace(machineID)) != 32 {
		return "", fmt.Errorf("machine ID %q isn't 32 hexadecimal digits", machineID)
	}

	sum := sha512.Sum384(id[:])

	hashed, err := uuid.FromBytes(sum[:16])
	if err != nil {
		return "", fmt.Errorf("hash machine ID: %w", err)
	}

	return hashed.String(), nil
}

// OSReleaseValue returns key's value in os-release content, as the agent reads
// it (parse_line in Trident's osutils/src/osrelease.rs): lines are KEY=VALUE,
// values may be quoted, and blank lines and comments are skipped. It reports
// false if the key is missing or empty.
func OSReleaseValue(content, key string) (string, bool) {
	for line := range strings.Lines(content) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		name, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(name) != key {
			continue
		}

		value = strings.TrimSpace(strings.Trim(strings.Trim(strings.TrimSpace(value), `"`), `'`))

		return value, value != ""
	}

	return "", false
}

// IsVersion reports whether version is strict semver, as Nebraska requires.
// The agent reports a version that isn't as 0.0.0, which Nebraska always
// offers an update to.
func IsVersion(version string) bool {
	_, err := semver.StrictNewVersion(version)

	return err == nil
}

// AgentInfo is the agent-info annotation: what centerpivot needs to send an
// update check as the node.
type AgentInfo struct {
	SchemaVersion string `json:"schemaVersion"`
	// MachineID is the node's Nebraska instance ID.
	MachineID string `json:"machineId"`
	// CurrentVersion is the version the agent reports to Nebraska. It's left
	// out when the agent couldn't read a valid one.
	CurrentVersion string `json:"currentVersion,omitempty"`
}

// NewAgentInfo returns the agent-info for a host's machine ID and current
// version. A version that isn't strict semver is left out.
func NewAgentInfo(machineID, version string) (AgentInfo, error) {
	instanceID, err := InstanceID(machineID)
	if err != nil {
		return AgentInfo{}, err
	}

	info := AgentInfo{SchemaVersion: AgentInfoSchemaVersion, MachineID: instanceID}
	if IsVersion(version) {
		info.CurrentVersion = version
	}

	return info, nil
}

// Marshal returns the annotation's value.
func (a AgentInfo) Marshal() (string, error) {
	data, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("encode agent-info: %w", err)
	}

	return string(data), nil
}

// ParseAgentInfo parses the annotation's value. A version that isn't strict
// semver is dropped, as if it were missing.
func ParseAgentInfo(value string) (AgentInfo, error) {
	var info AgentInfo
	if err := json.Unmarshal([]byte(value), &info); err != nil {
		return AgentInfo{}, fmt.Errorf("decode agent-info: %w", err)
	}

	if info.SchemaVersion != AgentInfoSchemaVersion {
		return AgentInfo{}, fmt.Errorf("unsupported agent-info schemaVersion %q", info.SchemaVersion)
	}

	if err := uuid.Validate(info.MachineID); err != nil {
		return AgentInfo{}, errors.New("agent-info machineId isn't a UUID")
	}

	if !IsVersion(info.CurrentVersion) {
		info.CurrentVersion = ""
	}

	return info, nil
}
