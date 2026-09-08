// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
)

// lldpAttributes mirrors the JSON stored in the neighbors attributes column.
type lldpAttributes struct {
	ChassisID         string `json:"chassisId"`
	PortID            string `json:"portId"`
	PortDescription   string `json:"portDesc"`
	SystemName        string `json:"systemName"`
	SystemDescription string `json:"systemDesc"`
	MgmtAddresses     string `json:"mgmtAddresses"`
}

// detectDuplicateLLDPUpstreamPorts finds cases where two different hosts report
// the same upstream switch port (same chassis + port) in their LLDP neighbor
// data, which typically indicates a wiring or configuration error.
func detectDuplicateLLDPUpstreamPorts(ctx context.Context, db *sql.DB) ([]conflictRecord, error) {
	const query = `SELECT host_identifier, local_interface, attributes FROM neighbors`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying neighbors: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	// upstreamKey -> list of "host (local_interface)" strings
	type neighborInfo struct {
		host           string
		localInterface string
	}

	upstream := make(map[string][]neighborInfo)

	for rows.Next() {
		var host, localIface, attrsRaw string

		if err := rows.Scan(&host, &localIface, &attrsRaw); err != nil {
			return nil, fmt.Errorf("scanning neighbor row: %w", err)
		}

		var attrs lldpAttributes
		if err := json.Unmarshal([]byte(attrsRaw), &attrs); err != nil {
			slog.Warn("skipping neighbor with unparseable attributes",
				"host", host,
				"local_interface", localIface,
				"error", err,
			)

			continue
		}

		if attrs.ChassisID == "" || attrs.PortID == "" {
			continue
		}

		key := attrs.ChassisID + "|" + attrs.PortID
		upstream[key] = append(upstream[key], neighborInfo{
			host:           host,
			localInterface: localIface,
		})
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating neighbor rows: %w", err)
	}

	var conflicts []conflictRecord

	for key, infos := range upstream {
		if len(infos) <= 1 {
			continue
		}

		parts := strings.SplitN(key, "|", 2)
		chassisID := parts[0]
		portID := parts[1]

		var devices []string
		for _, info := range infos {
			devices = append(devices, fmt.Sprintf("%s(%s)", info.host, info.localInterface))
		}

		conflicts = append(conflicts, conflictRecord{
			ConflictType: "duplicate_lldp_upstream_port",
			Devices:      fmt.Sprintf("chassis=%s port=%s hosts=[%s]", chassisID, portID, strings.Join(devices, ", ")),
		})

		slog.Warn("duplicate LLDP upstream port",
			"chassis_id", chassisID,
			"port_id", portID,
			"hosts", devices,
		)
	}

	return conflicts, nil
}
