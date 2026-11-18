// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

type nodeRebootTarget struct{}

func (nodeRebootTarget) Reference() string { return "machine-1" }
func (nodeRebootTarget) ObservedGeneration(context.Context, client.Reader) (int64, error) {
	return 0, nil
}

func (nodeRebootTarget) MatchSelector(ctx context.Context, r client.Reader, _ v1alpha3.OperationKind, selector labels.Selector) (bool, error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "node-1"}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, err
	}

	return selector.Matches(labels.Set(node.Labels)), nil
}

type fakeHostRebooter struct {
	bootID    string
	bootErr   error
	bootCalls int
	calls     int
}

func (h *fakeHostRebooter) BootID() (string, error) {
	h.bootCalls++

	return h.bootID, h.bootErr
}

func (h *fakeHostRebooter) RebootHost(context.Context, *slog.Logger) error {
	h.calls++

	return nil
}

func publicSoftOp() *v1alpha3.MachineOperation {
	return &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "soft"},
		Spec: v1alpha3.MachineOperationSpec{
			OperationKind: v1alpha3.OperationHostSoftReboot,
			MachineRef:    "machine-1",
		},
	}
}

func publicHandler(t *testing.T, c client.Client, reader client.Reader, host *fakeHostRebooter, flag *atomic.Bool) *HostSoftRebootHandler {
	t.Helper()

	h, err := NewHostSoftRebootHandler(c, reader, slog.New(slog.NewTextHandler(io.Discard, nil)), nodeRebootTarget{}, host, flag)
	require.NoError(t, err)

	return h
}

func publicOperation(t *testing.T, r client.Reader) v1alpha3.MachineOperation {
	t.Helper()

	var op v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: "soft"}, &op))

	return op
}

type rebootTestStore struct {
	operation MachineOperation
	result    MachineOperationResult[int64]
	err       error
	calls     int
}

func (*rebootTestStore) MarkInProgress(context.Context, MachineOperation, string) error {
	panic("unexpected MarkInProgress")
}

func (s *rebootTestStore) Finish(_ context.Context, op MachineOperation, result MachineOperationResult[int64]) error {
	s.calls++
	s.operation = op
	s.result = result

	return s.err
}

func TestHostSoftRebootBootFailureUsesCallerStore(t *testing.T) {
	for _, storeErr := range []error{nil, errors.New("store failed")} {
		c := fakeMachineOperationClient(publicSoftOp())
		host := &fakeHostRebooter{bootErr: errors.New("boot failed")}
		h := publicHandler(t, c, c, host, &atomic.Bool{})
		store := &rebootTestStore{err: storeErr}
		op := MachineOperation{Name: "soft", Kind: v1alpha3.OperationHostSoftReboot}

		_, err := h.Reconcile(t.Context(), store, op)
		assert.ErrorIs(t, err, storeErr)
		assert.Equal(t, 1, store.calls)
		assert.Equal(t, op, store.operation)
		assert.Equal(t, MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "ExecutionFailed",
			Message: "boot failed",
		}, store.result)
		assert.Empty(t, publicOperation(t, c).Status.Phase)
		assert.Zero(t, host.calls)
	}
}

func TestHostSoftRebootUsesClientForStatusAndStartup(t *testing.T) {
	c := fakeMachineOperationClient(publicSoftOp())
	reader := fakeMachineOperationClient()
	host := &fakeHostRebooter{bootID: "boot-1"}
	h := publicHandler(t, c, reader, host, &atomic.Bool{})

	_, err := h.Reconcile(t.Context(), &rebootTestStore{}, MachineOperation{Name: "soft"})
	require.NoError(t, err)
	require.Equal(t, 1, host.calls)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, publicOperation(t, c).Status.Phase)

	host.bootID = "boot-2"

	require.NoError(t, h.PublishStartup(t.Context()))
	require.Equal(t, v1alpha3.OperationPhaseComplete, publicOperation(t, c).Status.Phase)
}

func TestHostSoftRebootHandlerValidatesDependencies(t *testing.T) {
	c := fakeMachineOperationClient()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	host := &fakeHostRebooter{}

	flag := &atomic.Bool{}
	for _, tc := range []struct {
		name   string
		c      client.Client
		reader client.Reader
		log    *slog.Logger
		target HostSoftRebootTarget
		host   HostRebooter
		flag   *atomic.Bool
	}{
		{
			name:   "client",
			reader: c,
			log:    log,
			target: nodeRebootTarget{},
			host:   host,
			flag:   flag,
		},
		{
			name:   "reader",
			c:      c,
			log:    log,
			target: nodeRebootTarget{},
			host:   host,
			flag:   flag,
		},
		{
			name:   "logger",
			c:      c,
			reader: c,
			target: nodeRebootTarget{},
			host:   host,
			flag:   flag,
		},
		{
			name:   "target",
			c:      c,
			reader: c,
			log:    log,
			host:   host,
			flag:   flag,
		},
		{
			name:   "host",
			c:      c,
			reader: c,
			log:    log,
			target: nodeRebootTarget{},
			flag:   flag,
		},
		{
			name:   "typed nil host",
			c:      c,
			reader: c,
			log:    log,
			target: nodeRebootTarget{},
			host:   (*fakeHostRebooter)(nil),
			flag:   flag,
		},
		{
			name:   "flag",
			c:      c,
			reader: c,
			log:    log,
			target: nodeRebootTarget{},
			host:   host,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewHostSoftRebootHandler(tc.c, tc.reader, tc.log, tc.target, tc.host, tc.flag)
			require.Error(t, err)
		})
	}
}
