// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"log/slog"

	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

// ResolveHostPrefix returns the installation prefix this host was built with.
//
// Processes started by systemd, such as the daemon and the nspawn lifecycle
// hooks, cannot inherit the prefix from the environment that bootstrapped the
// host, so it has to be read back from disk. Two files carry it and they are
// written at different times, which is why this asks them in order:
//
// The ownership record is written before the first host mutation, so it is the
// only source that survives a bootstrap which failed before the node started.
// That case is not hypothetical: it is where teardown runs, and teardown is
// what has to find the agent's own files.
//
// The applied config is written once the node starts. It is the fallback for a
// host provisioned by an agent that predates the record carrying a prefix,
// where the record exists but the field does not.
//
// The default is what a host installed before any of this actually has on disk.
func ResolveHostPrefix(log *slog.Logger) string {
	if prefix := hostPrefixFromRecord(log, installstate.DefaultStore()); prefix != "" {
		return prefix
	}

	return goalstates.HostPrefixFromAppliedConfig(log)
}

// hostPrefixFromRecord returns the recorded prefix, or the empty string when
// there is no usable record to read one from.
//
// An absent record is ordinary: the host may predate the record entirely, or
// reset may have removed it. An unreadable one is not, and is worth saying out
// loud, because falling through lands on a prefix that is wrong precisely when
// the host configured one.
func hostPrefixFromRecord(log *slog.Logger, store *installstate.Store) string {
	r, err := store.Load()
	if err != nil {
		if log != nil && !errors.Is(err, installstate.ErrNotFound) {
			log.Warn("cannot read installation record while resolving the host prefix", "error", err)
		}

		return ""
	}

	return r.HostPrefix
}
