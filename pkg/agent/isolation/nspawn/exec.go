// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package nspawn is the isolation backend that runs the node stack in a
// systemd-nspawn machine. It owns every use of machinectl and
// systemd-run --machine so the rest of the agent stays backend neutral.
package nspawn

import (
	"context"
	"io"
	"log/slog"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation"
)

// MachinesDir is where systemd-nspawn machine root filesystems live.
const MachinesDir = "/var/lib/machines"

// Root returns the NodeRoot for the machine slot, rooted at its directory
// under MachinesDir.
func Root(slot string) isolation.DirRoot {
	return RootAt(slot, MachinesDir+"/"+slot)
}

// RootAt returns the NodeRoot for slot rooted at dir.
func RootAt(slot, dir string) isolation.DirRoot {
	return isolation.DirRoot{SlotName: slot, Dir: dir}
}

// Exec runs args inside the machine named by slot using
// systemd-run --machine=<slot> --pipe --wait. Stdout is streamed at Debug and
// stderr at Error, and the captured stdout is returned.
func Exec(ctx context.Context, log *slog.Logger, slot string, args ...string) (string, error) {
	runArgs := make([]string, 0, 3+len(args))
	runArgs = append(runArgs, "--machine="+slot, "--pipe", "--wait")
	runArgs = append(runArgs, args...)

	return executil.OutputCmd(ctx, log, "systemd-run", runArgs...)
}

// Logs returns the machine journal for the slot, via journalctl --machine.
func Logs(ctx context.Context, slot string, opts isolation.LogOptions) (io.ReadCloser, error) {
	args := []string{"--machine=" + slot, "--no-pager"}
	if opts.Unit != "" {
		args = append(args, "--unit="+opts.Unit)
	}

	args = append(args, isolation.JournalTailArgs(opts)...)

	return isolation.StartLogStream(ctx, "journalctl", args...)
}
