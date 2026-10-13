// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"errors"
	"log/slog"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/cmd/agent/internal/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

func newCmdDaemon(cmdCtx *CommandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Long-running daemon for node lifecycle management",
		Long: "Long-running daemon that manages the nspawn machine lifecycle. " +
			"Runs as a systemd unit after initial provisioning.",
		// systemd runs this with fixed arguments, so a failure here is never a
		// usage problem and the flag listing is noise in the journal.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer cancel()

			cmdCtx.Setup()

			err := daemon.Run(ctx, cmdCtx.Logger)

			restart := inPlaceRestart{
				currentPath: daemonCurrentPath,
				exec:        unix.Exec,
				args:        os.Args[1:],
				env:         os.Environ(),
			}

			return quietWhenDeferred(cmd, restart.after(cmdCtx.Logger, err))
		},
	}

	return cmd
}

// inPlaceRestart replaces a daemon that stopped to be restarted with the binary
// its unit runs, in the same process; see daemon.ErrRestartRequested.
//
// Re-executing keeps the process, so systemd sees no restart: there is no
// RestartSec delay, and the restart does not count toward the unit's start
// limit. It is still the unit's binary that runs: CurrentPath is what the
// unit's ExecStart names, and after an AgentUpgrade or a host root move it
// leads to the new binary or the moved files. If the new binary fails, it exits
// like any crashing daemon, and Restart= and OnFailure recovery take it from
// there.
type inPlaceRestart struct {
	// currentPath returns the path the daemon unit's ExecStart runs.
	currentPath func() (string, error)
	// exec replaces the process image. It returns only if that failed.
	exec func(argv0 string, argv, envv []string) error
	// args are the daemon's arguments after the program name, and env its
	// environment, both passed on unchanged.
	args, env []string
}

// after handles the result of daemon.Run. A daemon that stopped to be
// restarted is re-executed; if that cannot be done, it exits 0, and the unit's
// Restart=always starts it again after RestartSec. Any other result is
// returned unchanged.
func (r inPlaceRestart) after(log *slog.Logger, err error) error {
	if !errors.Is(err, daemon.ErrRestartRequested) {
		return err
	}

	path, err := r.currentPath()
	if err != nil {
		log.Warn("could not resolve the daemon unit's binary; exiting for systemd to restart the daemon",
			"unit", goalstates.DaemonUnit, "error", err)

		return nil
	}

	log.Info("re-executing the daemon in place", "path", path)

	if err := r.exec(path, append([]string{path}, r.args...), r.env); err != nil {
		log.Warn("could not re-execute the daemon; exiting for systemd to restart it",
			"unit", goalstates.DaemonUnit, "path", path, "error", err)
	}

	return nil
}

// daemonCurrentPath returns the path the daemon unit's ExecStart runs, under
// the host root as it resolves now.
func daemonCurrentPath() (string, error) {
	paths, err := goalstates.ResolvedAgentUpgradePaths()

	return paths.CurrentPath, err
}

// quietWhenDeferred stops cobra reporting a deferred daemon as an error.
//
// Standing down because an installation owns the host is an ordinary state
// that the daemon has already recorded as a warning. Letting cobra print
// "Error:" over the top of that puts a fault in the journal where there is
// none, which is exactly the confusion this whole path exists to remove.
func quietWhenDeferred(cmd *cobra.Command, err error) error {
	if errors.Is(err, daemon.ErrDeferred) {
		cmd.SilenceErrors = true
	}

	return err
}
