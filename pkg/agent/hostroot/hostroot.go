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
	// Path is where an agent's host-side files live. Its parent, /opt/unbounded,
	// is not the agent's: hosts stage files there, such as offline artifacts,
	// so it is created when missing and otherwise never changed or removed.
	Path = impl.Path

	// LegacyPath is where releases that predate Path installed them.
	LegacyPath = impl.LegacyPath
)

// Resolve returns the directory Path refers to on this host, with symlinks
// resolved: LegacyPath on a host Migrate linked, and otherwise Path with any
// link along it resolved, such as /opt being a link, or a link at Path that an
// operator made. A Path that does not exist yet resolves to where it will be
// once created.
//
// Paths built from it are compared with symlink targets, which are resolved,
// so they have to be resolved too. Building them from an unresolved Path on a
// migrated host would name /opt/unbounded/agent/bin/unbounded-agent-blue while
// the current link resolves to /usr/local/bin/unbounded-agent-blue, and the two
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
// It is idempotent and does nothing on a host without a legacy installation,
// or while a move by ReconcileMove is under way. The link is made inside Path's
// parent, which is created if missing and otherwise left as it is, along with
// anything else in it. It refuses a host with a legacy installation where Path
// is also a directory, because either could be the live one, and a host where
// Path is neither a directory nor a link. A link to LegacyPath with no
// installation behind it, left by an older release's reset, is removed so a
// fresh installation gets a real directory. A link to anywhere else is an
// operator's, and is kept.
//
// Commands that change the host call it first, before any path is resolved: a
// path resolved on an unmigrated legacy host names Path, where nothing is
// installed.
func Migrate(log *slog.Logger, markers ...string) error {
	return impl.Migrate(log, markers...)
}

// Prepare creates Path, its parent if missing, and the given subdirectories as
// a new installation needs them, with mode 0755 regardless of the umask, and
// restores the SELinux labels under Path where the policy tools are present.
// An existing parent keeps its mode. On a migrated host Path is the existing
// installation and is left as it is.
//
// The labels matter because a directory takes its parent's label when it is
// created. Under /opt that is usr_t, while the policy expects bin_t under
// /opt/.../bin; files created later inherit the directory's label.
func Prepare(ctx context.Context, log *slog.Logger, subdirs ...string) error {
	return impl.Prepare(ctx, log, subdirs...)
}

// LegacyReleased reports whether nothing an agent runs is under LegacyPath any
// more: Path is a real directory holding a finished installation, or a link an
// operator made that leads to a directory outside LegacyPath. Until it is,
// files under LegacyPath may still be in use, even ones the install scripts
// leave there for older releases, so an agent removes nothing there before it
// reports true. A binary left there for older releases is also what one of
// them would adopt after an AgentUpgrade back to it, so where it reports true
// that binary should be gone before such an upgrade can start.
func LegacyReleased() (bool, error) {
	return impl.LegacyReleased()
}

// MoveOptions describes what ReconcileMove moves and how the agent follows it:
// the agent's files and directories under the root, where binaries that know
// the host root are recorded, the AgentUpgrade signal and blue-green links that
// decide when the move is safe, how to check the daemon runs from the copy, and
// how to rewrite the units and restart the daemon.
type MoveOptions = impl.MoveOptions

// ReconcileMove moves a host a release before Path installed from LegacyPath
// into a real directory at Path, once neither the current nor the last-good
// binary predates Path, and finishes a move that was interrupted. Call it from
// the daemon after Migrate, while holding what keeps an upgrade or a reset from
// changing the layout.
//
// Each call on a linked host records the running binary's digest in
// MoveOptions.Record, which is how a binary that predates Path is told apart:
// it never records itself.
//
// A move spans two daemon starts. The first copies the files into Path,
// restores their SELinux labels, calls MoveOptions.Verify, then
// MoveOptions.RewriteUnits and MoveOptions.Restart, and reports true. The
// daemon must then stop taking work and wait to be replaced. The second start,
// running from Path, rewrites the units again and removes the files under
// LegacyPath. Nothing is removed while the daemon still runs from LegacyPath,
// so a restart that fails leaves every file in place and the next start tries
// again.
//
// A host that cannot run programs from Path stays linked. Where its filesystem
// is mounted noexec, nothing is copied. Where Verify fails, the copy is
// replaced with the link again, RewriteUnits is called to point the units back
// at LegacyPath, and the error is returned. The next start tries again.
func ReconcileMove(ctx context.Context, log *slog.Logger, opts MoveOptions) (bool, error) {
	return impl.ReconcileMove(ctx, log, opts)
}
