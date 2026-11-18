// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
)

type hostSoftRebootTarget struct {
	machineName string
	nodeName    string
}

func (t hostSoftRebootTarget) Reference() string { return t.machineName }

func (t hostSoftRebootTarget) ObservedGeneration(ctx context.Context, reader client.Reader) (int64, error) {
	machine, err := getLocalMachine(ctx, reader, t.machineName)
	if err != nil {
		return 0, err
	}

	return machine.Generation, nil
}

func (t hostSoftRebootTarget) MatchSelector(
	ctx context.Context,
	reader client.Reader,
	kind v1alpha3.OperationKind,
	selector labels.Selector,
) (bool, error) {
	if kind == v1alpha3.OperationHostSoftReboot {
		var node corev1.Node
		if err := reader.Get(ctx, client.ObjectKey{Name: t.nodeName}, &node); err == nil {
			return selector.Matches(labels.Set(node.Labels)), nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get Node %s for selector match: %w", t.nodeName, err)
		}
	}

	var machine v1alpha3.Machine
	if err := reader.Get(ctx, client.ObjectKey{Name: t.machineName}, &machine); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}

		return false, fmt.Errorf("get Machine %s for selector match: %w", t.machineName, err)
	}

	return selector.Matches(labels.Set(machine.Labels)), nil
}

func newHostSoftRebootHandler(
	c client.Client,
	reader client.Reader,
	log *slog.Logger,
	machineName, nodeName string,
	host daemon.HostRebooter,
	requested *atomic.Bool,
) (*daemon.HostSoftRebootHandler, error) {
	return daemon.NewHostSoftRebootHandler(
		c,
		reader,
		log,
		hostSoftRebootTarget{
			machineName: machineName,
			nodeName:    nodeName,
		},
		host,
		requested,
	)
}

func (t *machineOperationTarget) reconcileHostSoftReboot(ctx context.Context, _ daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	if t.rebootRequested.Load() {
		return ctrl.Result{}, nil
	}

	lock, err := t.installation.AcquireMutationLock()
	if errors.Is(err, installstate.ErrLockHeld) {
		return ctrl.Result{RequeueAfter: agentUpgradeLockRetryDelay}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	defer releaseInstallationLock(t.log, lock)

	// Native requests require the local Machine even when waiting or when a
	// boot-ID read fails. Resolve its generation again during the start CAS.
	if _, err := getLocalMachine(ctx, t.reader(), t.machineName); err != nil {
		return ctrl.Result{}, err
	}

	handler, err := newHostSoftRebootHandler(
		t.Client,
		t.reader(),
		t.log,
		t.machineName,
		t.nodeName,
		t.nodeOperator,
		&t.rebootRequested,
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	return handler.Reconcile(ctx, op)
}

// reader returns the reader for checks that must not act on a stale cache.
func (t *machineOperationTarget) reader() client.Reader {
	if t.apiReader != nil {
		return t.apiReader
	}

	return t.Client
}
