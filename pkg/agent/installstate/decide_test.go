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

	installing := Record{MachineName: machine, ConfigFingerprint: fp, Stage: StageInstalling}

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
			rec:  installing,
			want: DispositionResume,
		},
		{
			name: "completed install is a no-op",
			rec:  Record{MachineName: machine, ConfigFingerprint: fp, Stage: StageComplete},
			want: DispositionAlreadyComplete,
		},
		{
			name: "interrupted teardown refuses",
			rec:  Record{MachineName: machine, ConfigFingerprint: fp, Stage: StageResetting},
			want: DispositionRefuse,
		},
		{
			name: "another machine's install refuses",
			rec:  Record{MachineName: "other", ConfigFingerprint: fp, Stage: StageInstalling},
			want: DispositionRefuse,
		},
		{
			// Resuming under changed config would apply half of one intent and
			// half of another.
			name: "changed configuration refuses",
			rec:  Record{MachineName: machine, ConfigFingerprint: "different", Stage: StageInstalling},
			want: DispositionRefuse,
		},
		{
			name: "unreadable record refuses rather than reading as absent",
			err:  errors.New("permission denied"),
			want: DispositionRefuse,
		},
		{
			name: "unrecognized stage refuses",
			rec:  Record{MachineName: machine, ConfigFingerprint: fp, Stage: Stage("weird")},
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

// TestFingerprintDistinguishesConfigs keeps the resume guard meaningful.
func TestFingerprintDistinguishesConfigs(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Fingerprint([]byte(`{"a":1}`)), Fingerprint([]byte(`{"a":1}`)))
	assert.NotEqual(t, Fingerprint([]byte(`{"a":1}`)), Fingerprint([]byte(`{"a":2}`)))
}

func TestNewInstallIDIsUnique(t *testing.T) {
	t.Parallel()

	first, err := NewInstallID()
	assert.NoError(t, err)

	second, err := NewInstallID()
	assert.NoError(t, err)

	assert.NotEqual(t, first, second)
	assert.Len(t, first, 32)
}

// TestDecideChecksIdentityOnCompletedInstalls covers a completed record that no
// longer matches what the caller intends to install.
//
// Returning success there would let a reconfigured host, or one whose record
// survived an incomplete teardown, skip bootstrap and come up as whatever it
// used to be.
func TestDecideChecksIdentityOnCompletedInstalls(t *testing.T) {
	t.Parallel()

	const (
		machine = "node-1"
		fp      = "fingerprint-1"
	)

	complete := Record{MachineName: machine, ConfigFingerprint: fp, Stage: StageComplete}

	assert.Equal(t, DispositionAlreadyComplete,
		Decide(complete, nil, machine, fp).Disposition)

	wrongMachine := complete
	wrongMachine.MachineName = "someone-else"

	got := Decide(wrongMachine, nil, machine, fp)
	assert.Equal(t, DispositionRefuse, got.Disposition)
	assert.Contains(t, got.Reason, "already bootstrapped")

	wrongConfig := complete
	wrongConfig.ConfigFingerprint = "different"

	got = Decide(wrongConfig, nil, machine, fp)
	assert.Equal(t, DispositionRefuse, got.Disposition)
	assert.Contains(t, got.Reason, "different agent configuration")
}
