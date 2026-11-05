// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/migrate"
)

// twoSlotHost writes applied configs for both nspawn slots, as a repave that
// stopped between recording the new slot and removing the old one leaves them.
func twoSlotHost(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	writeAppliedFixture(t, dir, "kube1", baseConfig())
	writeAppliedFixture(t, dir, "kube2", baseConfig())

	return dir
}

func runningSlots(up map[string]bool, err error) func(context.Context, *slog.Logger, *ActiveMachine) (bool, error) {
	return func(_ context.Context, _ *slog.Logger, active *ActiveMachine) (bool, error) {
		return up[active.Name], err
	}
}

func TestFindActiveMachine_TwoRecords(t *testing.T) {
	tests := []struct {
		name    string
		running func(context.Context, *slog.Logger, *ActiveMachine) (bool, error)
		journal *migrate.Journal
		want    string
		wantErr string
	}{
		{name: "nothing to ask fails closed", wantErr: "nothing says which one is active"},
		{name: "the running stack is active", running: runningSlots(map[string]bool{"kube2": true}, nil), want: "kube2"},
		{name: "neither running fails closed", running: runningSlots(nil, nil), wantErr: "0 of them running"},
		{name: "both running fails closed", running: runningSlots(map[string]bool{"kube1": true, "kube2": true}, nil), wantErr: "2 of them running"},
		{name: "an unanswered question fails closed", running: runningSlots(nil, errors.New("machinectl failed")), wantErr: "machinectl failed"},
		{
			name: "a journal before the switch names the source",
			journal: &migrate.Journal{
				Strategy: migrate.InPlace, From: isolation.ModeNSpawn, To: isolation.ModeNSpawn,
				SourceSlot: "kube2", TargetSlot: "kube1", Step: migrate.StepStartingTarget,
			},
			running: runningSlots(nil, errors.New("must not be asked")),
			want:    "kube2",
		},
		{
			name: "a journal after the switch names the target",
			journal: &migrate.Journal{
				Strategy: migrate.InPlace, From: isolation.ModeNSpawn, To: isolation.ModeNSpawn,
				SourceSlot: "kube2", TargetSlot: "kube1", Step: migrate.StepAwaitingReady,
			},
			want: "kube1",
		},
		{
			name:    "a reprovision journal names no slot",
			journal: &migrate.Journal{Strategy: migrate.Reprovision, From: isolation.ModeNSpawn, To: isolation.ModeNone, Step: migrate.StepReprovisioning},
			wantErr: "nothing says which one is active",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := twoSlotHost(t)

			if tt.journal != nil {
				require.NoError(t, migrate.JournalStore{Path: inPlaceJournalPathIn(dir)}.Save(*tt.journal))
			}

			active, err := hostOperator{configDir: dir, slotRunning: tt.running}.FindActiveMachine(discardLogger())
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				require.ErrorContains(t, err, "needs attention")

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, active.Name)
		})
	}
}

func TestFindActiveMachine_OneRecordNeedsNoQuestion(t *testing.T) {
	dir := t.TempDir()
	writeAppliedFixture(t, dir, "kube2", baseConfig())

	active, err := hostOperator{configDir: dir, slotRunning: runningSlots(nil, errors.New("must not be asked"))}.FindActiveMachine(discardLogger())
	require.NoError(t, err)
	assert.Equal(t, "kube2", active.Name)
}

// A repave over two records it cannot tell apart must not touch either: it
// would rebuild the slot that runs, as the stale one.
func TestReconcileRepave_TwoUndecidableRecordsDoNotRepave(t *testing.T) {
	dir := twoSlotHost(t)
	nspawnOp := &fakeNodeOperator{}

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}
	r := &repaveReconciler{
		installation: installstate.NewStore(filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "install.lock")),
		Client:       fakeStatusClient(machine),
		log:          discardLogger(),
		machineName:  "test-machine",
		nodeOperator: newDispatchOperator(hostOperator{configDir: dir}, ModeOperators{isolation.ModeNSpawn: nspawnOp}),
	}

	_, err := r.ReconcileRepave(context.Background(), "node-delete")
	require.ErrorContains(t, err, "needs attention")
	assert.Nil(t, nspawnOp.repaveActive, "no slot may be repaved")
	assert.False(t, nspawnOp.restartCalled)
}

func TestModeBackendsRunning(t *testing.T) {
	up := &isolation.FakeBackend{FakeStatus: isolation.Status{Running: true}}
	backends := ModeBackends{isolation.ModeNSpawn: func(*slog.Logger, *provision.AgentConfig) isolation.Backend { return up }}

	running, err := backends.running(context.Background(), discardLogger(), &ActiveMachine{Name: "kube1"})
	require.NoError(t, err)
	assert.True(t, running)
	assert.Equal(t, []string{"status kube1"}, up.Calls())

	_, err = backends.running(context.Background(), discardLogger(), &ActiveMachine{Name: "kube1", Mode: isolation.ModeNone})
	require.ErrorContains(t, err, "no backend")
}
