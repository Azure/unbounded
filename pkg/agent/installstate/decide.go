// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"fmt"
)

// Disposition is what bootstrap should do about the host it found.
type Disposition int

const (
	// DispositionFresh means no installation is recorded: bootstrap must
	// verify the host is clean and then start a new one.
	DispositionFresh Disposition = iota

	// DispositionResume means the record describes this same installation,
	// left unfinished. Bootstrap continues from its recorded checkpoint and
	// must not require a clean host.
	DispositionResume

	// DispositionAlreadyComplete means bootstrap has already finished here.
	// The caller still verifies the completion marker before believing it.
	DispositionAlreadyComplete

	// DispositionRefuse means the host carries state bootstrap must not touch.
	DispositionRefuse
)

// Decision is the outcome of inspecting the host's installation state.
type Decision struct {
	Disposition Disposition
	// Record is the existing record for Resume and AlreadyComplete.
	Record Record
	// Reason explains a refusal, and is what the operator sees.
	Reason string
}

// Decide reports what bootstrap should do for the given machine and config.
//
// This is the whole of the resume policy, kept as a pure function so every
// branch can be tested without a host. The rule is that bootstrap may only ever
// continue over artifacts it can prove are its own: same machine, same
// configuration, and an install that was not already being torn down.
func Decide(rec Record, err error, machineName, fingerprint string) Decision {
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Decision{Disposition: DispositionFresh}
		}

		// An unreadable record may still describe files on this host, so this
		// is not the same as having none.
		return Decision{
			Disposition: DispositionRefuse,
			Reason: fmt.Sprintf(
				"installation record could not be read (%v); "+
					"inspect it, then run 'unbounded-agent reset' to clear it",
				err,
			),
		}
	}

	if rec.Checkpoint == CheckpointResetting {
		return Decision{
			Disposition: DispositionRefuse,
			Record:      rec,
			Reason: "a previous reset did not finish; " +
				"run 'unbounded-agent reset' again before bootstrapping",
		}
	}

	// Identity is checked before the checkpoint, and for completed installs as
	// well as unfinished ones. Skipping it on a completed record would let a
	// host that was reconfigured, or whose record survived an incomplete
	// teardown, come back up as whatever it used to be.
	if mismatch := identityMismatch(rec, machineName, fingerprint); mismatch != "" {
		return Decision{
			Disposition: DispositionRefuse,
			Record:      rec,
			Reason: fmt.Sprintf(
				"host already carries an installation, but %s; "+
					"run 'unbounded-agent reset' first",
				mismatch,
			),
		}
	}

	if rec.Checkpoint == CheckpointComplete {
		return Decision{Disposition: DispositionAlreadyComplete, Record: rec}
	}

	return Decision{Disposition: DispositionResume, Record: rec}
}

// identityMismatch describes how a record differs from what the caller intends
// to install, or returns an empty string when they agree.
func identityMismatch(rec Record, machineName, fingerprint string) string {
	if rec.MachineName != machineName {
		return fmt.Sprintf("it is for machine %q, not %q", rec.MachineName, machineName)
	}

	if rec.ConfigFingerprint != fingerprint {
		// Continuing under changed configuration would apply half of one intent
		// and half of another.
		return "it was made for a different agent configuration"
	}

	return ""
}
