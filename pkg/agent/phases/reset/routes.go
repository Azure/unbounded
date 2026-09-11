// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package reset

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// wireguardTableStart and wireguardTableEnd define the range of routing table
// IDs used by unbounded-net WireGuard gateways.
const (
	wireguardTableStart = 51820
	wireguardTableEnd   = 51899
)

type cleanupRoutes struct {
	log    *slog.Logger
	output func(context.Context, ...string) (string, error)
}

// CleanupRoutes returns a task that removes policy routing rules and flushes
// routing tables used by unbounded-net WireGuard gateways.
func CleanupRoutes(log *slog.Logger) phases.Task {
	return &cleanupRoutes{log: log}
}

func (t *cleanupRoutes) Name() string { return "cleanup-routes" }

func (t *cleanupRoutes) Do(ctx context.Context) error {
	t.log.Info("cleaning up policy routing rules")

	output := t.output
	if output == nil {
		output = func(ctx context.Context, args ...string) (string, error) {
			return executil.OutputCmd(ctx, t.log, "ip", args...)
		}
	}

	for _, family := range []string{"-4", "-6"} {
		for _, kind := range []string{"rule", "route"} {
			args := []string{family, "-N", "-j", kind, "show"}
			if kind == "route" {
				args = append(args, "table", "all")
			}

			out, err := output(ctx, args...)
			if err != nil {
				return fmt.Errorf("inspect %s %s: %w", family, kind, err)
			}

			tables, err := ownedRoutingTables(out)
			if err != nil {
				return fmt.Errorf("decode %s %s: %w", family, kind, err)
			}

			flushed := make(map[int]bool)

			for _, table := range tables {
				action := "del"

				if kind == "route" {
					if flushed[table] {
						continue
					}

					flushed[table] = true
					action = "flush"
				}

				if _, err := output(ctx, family, kind, action, "table", strconv.Itoa(table)); err != nil {
					return fmt.Errorf("remove %s %s table %d: %w", family, kind, table, err)
				}
			}
		}
	}

	return nil
}

// Numeric ip output keeps locally named routing tables from hiding owned IDs.
// Enumerating first distinguishes genuine absence from a failed mutation.
func ownedRoutingTables(output string) ([]int, error) {
	var entries []struct {
		Table json.RawMessage `json:"table"`
	}
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return nil, err
	}

	var tables []int

	for _, entry := range entries {
		var table int

		if len(entry.Table) == 0 {
			continue
		}

		if err := json.Unmarshal(entry.Table, &table); err != nil {
			var name string
			if err := json.Unmarshal(entry.Table, &name); err != nil {
				return nil, err
			}

			var parseErr error

			table, parseErr = strconv.Atoi(name)
			if parseErr != nil {
				switch name {
				case "main", "local", "default", "unspec":
					continue
				}

				return nil, fmt.Errorf("non-numeric routing table %q", name)
			}
		}

		if table >= wireguardTableStart && table <= wireguardTableEnd {
			tables = append(tables, table)
		}
	}

	return tables, nil
}
