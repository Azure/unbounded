package reset

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
)

// wireguardTableStart and wireguardTableEnd define the range of routing table
// IDs used by unbounded-net WireGuard gateways.
const (
	wireguardTableStart = 51820
	wireguardTableEnd   = 51899
)

type cleanupRoutes struct {
	log *slog.Logger
}

// CleanupRoutes returns a task that removes policy routing rules and flushes
// routing tables used by unbounded-net WireGuard gateways.
func CleanupRoutes(log *slog.Logger) phases.Task {
	return &cleanupRoutes{log: log}
}

func (t *cleanupRoutes) Name() string { return "cleanup-routes" }

func (t *cleanupRoutes) Do(ctx context.Context) error {
	t.log.Info("cleaning up policy routing rules")

	for table := wireguardTableStart; table <= wireguardTableEnd; table++ {
		tableStr := fmt.Sprintf("%d", table)

		// Remove all ip rules pointing to this table.
		for {
			cmd := exec.CommandContext(ctx, "ip", "rule", "del", "table", tableStr)
			if err := cmd.Run(); err != nil {
				break // no more rules for this table
			}
		}

		// Flush the routing table.
		cmd := exec.CommandContext(ctx, "ip", "route", "flush", "table", tableStr)
		_ = cmd.Run()
	}

	return nil
}
