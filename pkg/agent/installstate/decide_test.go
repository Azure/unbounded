// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDecide covers the resume policy, which is the whole point of the record:
// bootstrap may continue over artifacts only when it can prove they are its
// own, and must otherwise refuse rather than adopt or destroy them.
func TestDecide(t *testing.T) {
	t.Parallel()

	const (
		machine = "node-1"
		fp      = "fingerprint-1"
	)

	base := Record{MachineName: machine, ConfigFingerprint: fp}

	at := func(c Checkpoint) Record {
		r := base
		r.Checkpoint = c

		return r
	}

	tests := []struct {
		name string
		rec  Record
		err  error
		want Disposition
	}{
		{
			name: "clean host starts a fresh install",
			err:  ErrNotFound,
			want: DispositionFresh,
		},
		{
			// The failure this whole change exists for: a download died after
			// the workspace was created, and every retry used to be rejected.
			name: "own unfinished install resumes",
			rec:  at(CheckpointPreparingRootFS),
			want: DispositionResume,
		},
		{
			name: "install interrupted after the node started resumes",
			rec:  at(CheckpointInstallingDaemon),
			want: DispositionResume,
		},
		{
			name: "completed install is a no-op",
			rec:  at(CheckpointComplete),
			want: DispositionAlreadyComplete,
		},
		{
			name: "interrupted teardown refuses",
			rec:  at(CheckpointResetting),
			want: DispositionRefuse,
		},
		{
			name: "unreadable record refuses rather than reading as absent",
			err:  errors.New("permission denied"),
			want: DispositionRefuse,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Decide(tc.rec, tc.err, machine, fp)
			assert.Equal(t, tc.want, got.Disposition)

			if tc.want == DispositionRefuse {
				assert.NotEmpty(t, got.Reason, "a refusal must tell the operator what to do")
			}
		})
	}
}

// TestDecideChecksIdentityAtEveryCheckpoint covers a record that no longer
// matches what the caller intends to install.
//
// This has to hold for a completed record too. Returning success there would
// let a reconfigured host, or one whose record survived an incomplete teardown,
// skip bootstrap and come up as whatever it used to be.
func TestDecideChecksIdentityAtEveryCheckpoint(t *testing.T) {
	t.Parallel()

	const (
		machine = "node-1"
		fp      = "fingerprint-1"
	)

	for _, checkpoint := range []Checkpoint{
		CheckpointPreparingHost,
		CheckpointPreparingRootFS,
		CheckpointStartingNode,
		CheckpointInstallingDaemon,
		CheckpointComplete,
	} {
		t.Run(string(checkpoint), func(t *testing.T) {
			t.Parallel()

			wrongMachine := Record{
				MachineName:       "someone-else",
				ConfigFingerprint: fp,
				Checkpoint:        checkpoint,
			}

			got := Decide(wrongMachine, nil, machine, fp)
			assert.Equal(t, DispositionRefuse, got.Disposition)
			assert.Contains(t, got.Reason, "not \"node-1\"")

			wrongConfig := Record{
				MachineName:       machine,
				ConfigFingerprint: "different",
				Checkpoint:        checkpoint,
			}

			got = Decide(wrongConfig, nil, machine, fp)
			assert.Equal(t, DispositionRefuse, got.Disposition)
			assert.Contains(t, got.Reason, "different agent configuration")
		})
	}
}
