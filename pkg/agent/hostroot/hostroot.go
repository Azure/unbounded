// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package hostroot locates the directory that holds an agent's own host-side
// files: its binaries and the helpers systemd units run. It does not cover the
// config, state, logs, units, or anything inside the nspawn machine.
//
// New installations use Path, which is writable on every supported host,
// including those that mount /usr read-only such as Azure Container Linux.
//
// Hosts installed by a release that predates Path keep their files under
// LegacyPath, and Migrate points Path at them with a symlink. Build every path
// from Resolve, so on such a host it names the files where they already are,
// and the units and links the older release wrote stay valid for it as well as
// for the new one. That is what lets an upgrade from the older release roll
// back to it. Once no older release is left to roll back to, ReconcileMove
// moves the files into a real directory at Path.
package hostroot

import (
	"context"
	"log/slog"

	impl "github.com/Azure/unbounded/internal/hostroot"
)

const (
	// Path is where an agent's host-side files live.
	Path = impl.Path

	// LegacyPath is where releases that predate Path installed them.
	LegacyPath = impl.LegacyPath
)

// Resolve returns the directory Path refers to on this host, with symlinks
// resolved: LegacyPath on a migrated host, and Path itself on any other.
//
// Paths built from it are compared with symlink targets, which are resolved,
// so they have to be resolved too. Building them from an unresolved Path on a
// migrated host would name /opt/unbounded/bin/unbounded-agent-blue while the
// current link resolves to /usr/local/bin/unbounded-agent-blue, and the two
// would never compare equal.
func Resolve() string {
	return impl.Resolve()
}

// Planned returns the directory Resolve will return once Migrate has run with
// the same markers. It changes nothing, so code that must not change the host,
// such as preflight, can ask where things will be.
func Planned(markers ...string) string {
	return impl.Planned(markers...)
}

// Migrate points Path at LegacyPath on a host whose installation is under
// LegacyPath. markers are paths relative to the root whose presence under
// LegacyPath identifies such an installation: the product's own binary layout,
// not files a fresh installation also creates there.
//
// It is idempotent and does nothing on a host without a legacy installation.
// It refuses a host with a legacy installation where Path is also a directory,
// because either could be the live one. A link to LegacyPath with no
// installation behind it, left by an older release's reset, is removed so a
// fresh installation gets a real directory.
//
// Commands that change the host call it first, before any path is resolved: a
// path resolved on an unmigrated legacy host names Path, where nothing is
// installed.
func Migrate(log *slog.Logger, markers ...string) error {
	return impl.Migrate(log, markers...)
}

// Prepare creates Path and the given subdirectories as a new installation
// needs them, with mode 0755 regardless of the umask, and restores their
// SELinux labels where the policy tools are present. On a migrated host Path
// is the existing installation and is left as it is.
//
// The labels matter because a directory takes its parent's label when it is
// created. Under /opt that is usr_t, while the policy expects bin_t under
// /opt/*/bin; files created later inherit the directory's label.
func Prepare(ctx context.Context, log *slog.Logger, subdirs ...string) error {
	return impl.Prepare(ctx, log, subdirs...)
}

// MoveOptions describes what ReconcileMove moves and how the agent follows it:
// the agent's files and directories under the root, where binaries that know
// the host root are recorded, the AgentUpgrade signal and blue-green links that
// decide when the move is safe, and how to rewrite the units and restart the
// daemon.
type MoveOptions = impl.MoveOptions

// ReconcileMove moves a host a release before Path installed from LegacyPath
// into a real directory at Path, once neither the current nor the last-good
// binary predates Path, and finishes a move that was interrupted. Call it from
// the daemon after Migrate, while holding what keeps an upgrade or a reset from
// changing the layout. It reports whether it restarted the daemon.
//
// Each call on a linked host records the running binary's digest in
// MoveOptions.Record, which is how a binary that predates Path is told apart:
// it never records itself. The files under LegacyPath stay until
// MoveOptions.RewriteUnits has pointed the units at the new ones.
func ReconcileMove(ctx context.Context, log *slog.Logger, opts MoveOptions) (bool, error) {
	return impl.ReconcileMove(ctx, log, opts)
}
