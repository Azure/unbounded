// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

const (
	hostSoftRebootMessage      = "rebooting host operating system"
	hostSoftRebootWaitInterval = 10 * time.Second
)

type testHostRebooter struct {
	bootID func() (string, error)
}

type staleHostReadClient struct {
	client.Client
	reader client.Reader
}

func (c staleHostReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func (c staleHostReadClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	return c.reader.List(ctx, list, opts...)
}

func (h testHostRebooter) BootID() (string, error)                      { return h.bootID() }
func (testHostRebooter) RebootHost(context.Context, *slog.Logger) error { return nil }

func publishHostSoftReboots(ctx context.Context, log *slog.Logger, c client.Client, machineName string, currentBootID func() (string, error)) error {
	handler, err := newHostSoftRebootHandler(c, c, log, machineName, "test-node", testHostRebooter{bootID: currentBootID}, &atomic.Bool{})
	if err != nil {
		return err
	}

	return handler.PublishStartup(ctx)
}

// Exercise the public protocol with a synthetic soft operation to observe the
// blocker selected by the real OOB check.
func unfinishedOutOfBandHostOperation(ctx context.Context, reader client.Reader, machine *v1alpha3.Machine) (string, error) {
	c, ok := reader.(client.Client)
	if !ok {
		return "", fmt.Errorf("test reader is not a client")
	}

	local := machine.DeepCopy()
	local.ResourceVersion = ""

	created := c.Create(ctx, local)
	if created != nil && !apierrors.IsAlreadyExists(created) {
		return "", created
	}

	if created == nil {
		defer func() { _ = c.Delete(ctx, local) }()
	}

	soft := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "test-soft-probe"},
		Spec: v1alpha3.MachineOperationSpec{
			MachineRef:    machine.Name,
			OperationKind: v1alpha3.OperationHostSoftReboot,
		},
	}
	if err := c.Create(ctx, soft); err != nil {
		return "", err
	}
	defer func() { _ = c.Delete(ctx, soft) }()

	host := &fakeNodeOperator{bootID: "test-boot"}

	handler, err := newHostSoftRebootHandler(c, reader, discardLogger(), machine.Name, "test-node", host, &atomic.Bool{})
	if err != nil {
		return "", err
	}

	result, err := handler.Reconcile(ctx, daemon.MachineOperation{Name: soft.Name})
	if err != nil {
		return "", err
	}

	if result.RequeueAfter == 0 {
		if host.rebootCalls != 1 {
			return "", fmt.Errorf("unblocked probe issued %d reboot commands, want 1", host.rebootCalls)
		}

		return "", nil
	}

	if host.rebootCalls != 0 {
		return "", fmt.Errorf("blocked probe issued %d reboot commands, want 0", host.rebootCalls)
	}

	if err := reader.Get(ctx, client.ObjectKeyFromObject(soft), soft); err != nil {
		return "", err
	}

	return strings.TrimPrefix(soft.Status.Message, "waiting for out-of-band host operation "), nil
}

func isOutOfBandHostOperation(t *testing.T, kind v1alpha3.OperationKind) bool {
	t.Helper()

	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "test-machine"}}
	op := outOfBandOperation("candidate", machine.Name, kind)
	blocker, err := unfinishedOutOfBandHostOperation(context.Background(), fakeStatusClient(op), machine)
	require.NoError(t, err)

	return blocker == op.Name
}

func hostOperationUnfinishedOn(t *testing.T, op *v1alpha3.MachineOperation, machine *v1alpha3.Machine) bool {
	t.Helper()

	blocker, err := unfinishedOutOfBandHostOperation(context.Background(), fakeStatusClient(op.DeepCopy()), machine)
	require.NoError(t, err)

	return blocker == op.Name
}

func markHostSoftRebootWaiting(ctx context.Context, c client.Client, name, message string) error {
	blockerName := strings.TrimPrefix(message, "waiting for out-of-band host operation ")

	blocker := outOfBandOperation(blockerName, "test-machine", v1alpha3.OperationHostHardReboot)
	if err := c.Create(ctx, blocker); err != nil {
		return err
	}

	defer func() { _ = c.Delete(ctx, blocker) }()

	handler, err := newHostSoftRebootHandler(c, c, discardLogger(), "test-machine", "test-node", testHostRebooter{bootID: staticBootID("test-boot")}, &atomic.Bool{})
	if err != nil {
		return err
	}

	_, err = handler.Reconcile(ctx, daemon.MachineOperation{Name: name})

	return err
}
