// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package installstate owns the agent's installation ownership record and the
// lock that guards it. The record is internal bookkeeping, not a configuration
// input.
//
// It answers three questions nothing else on the host can answer:
//
//   - Is there an installation here, and is it the one we are being asked to
//     perform? The record carries the machine name and a fingerprint of the
//     identity-defining configuration, so a retry can tell its own interrupted
//     attempt from a different installation. Without it, bootstrap can only
//     demand a pristine host and abort otherwise.
//   - How far did the last attempt get? The checkpoint lets a retry replay only
//     unfinished stages instead of redoing completed work or refusing outright.
//   - Is anything else mutating this host? The lock serializes bootstrap, reset,
//     repave, NodeReboot, host agent activation and AgentUpgrade, which all
//     touch the same files and services.
//
// It is separate from the bootstrap coordinator because the daemon's repave,
// NodeReboot, AgentUpgrade and AgentReset paths, reset, and host agent
// activation all need the record or the lock without running a bootstrap.
package installstate
