package reset

import (
	"context"
	"log/slog"

	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/goalstates"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/phases"
	"github.com/project-unbounded/unbounded-kube/cmd/agent/internal/utilexec"
)

const nftablesFlushUnit = "nftables-flush.service"

type removeNFTables struct {
	log *slog.Logger
}

// RemoveNFTables returns a task that disables the nftables-flush service,
// removes the unit file, and removes the unbounded-kube config directory.
func RemoveNFTables(log *slog.Logger) phases.Task {
	return &removeNFTables{log: log}
}

func (t *removeNFTables) Name() string { return "remove-nftables" }

func (t *removeNFTables) Do(ctx context.Context) error {
	t.log.Info("removing nftables flush service")

	// Disable and stop the unit (ignore errors if not installed).
	_ = utilexec.RunCmd(ctx, t.log, utilexec.Systemctl(), "disable", "--now", nftablesFlushUnit)

	removeFileIfExists(goalstates.SystemdSystemDir + "/" + nftablesFlushUnit)
	removeAllIfExists(goalstates.ConfigDir) //nolint:errcheck // best-effort cleanup

	return nil
}
