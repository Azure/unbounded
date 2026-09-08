// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package inspector

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/lib/pq"
)

// detectDuplicateSerials finds serial numbers that appear on more than one
// host_identifier for the same device type, indicating that two different hosts
// reported the same serial for the same class of hardware. Serial numbers are
// scoped per device type so that unrelated device classes (e.g. a GPU and a
// NIC) sharing a serial are not treated as conflicts.
func detectDuplicateSerials(ctx context.Context, db *sql.DB) ([]conflictRecord, error) {
	const query = `
SELECT device_type, serial_number, array_agg(DISTINCT host_identifier) AS hosts
FROM inventory
GROUP BY device_type, serial_number
HAVING COUNT(DISTINCT host_identifier) > 1`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying duplicate serials: %w", err)
	}
	defer rows.Close() //nolint:errcheck

	var conflicts []conflictRecord

	for rows.Next() {
		var (
			deviceType string
			serial     string
			hosts      []string
		)

		if err := rows.Scan(&deviceType, &serial, pq.Array(&hosts)); err != nil {
			return nil, fmt.Errorf("scanning duplicate serial row: %w", err)
		}

		conflicts = append(conflicts, conflictRecord{
			ConflictType: "duplicate_serial_number",
			Devices:      fmt.Sprintf("type=%s serial=%s hosts=[%s]", deviceType, serial, strings.Join(hosts, ", ")),
		})

		slog.Warn("duplicate serial number",
			"device_type", deviceType,
			"serial", serial,
			"hosts", hosts,
		)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duplicate serial rows: %w", err)
	}

	return conflicts, nil
}
