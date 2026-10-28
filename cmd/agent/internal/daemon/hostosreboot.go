// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/cmd/agent/internal/installstate"
	"github.com/Azure/unbounded/internal/executil"
	daemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// hostOSRebootSignal records a HostOSReboot the daemon has issued. The
// daemon that starts after the reboot reports the operation from it.
type hostOSRebootSignal struct {
	OperationName             string `json:"operationName"`
	ObservedMachineGeneration int64  `json:"observedMachineGeneration,omitempty"`
	// BootID is the kernel boot ID observed before the reboot was issued.
	BootID string `json:"bootID"`
}

type hostOSRebootSignalStore struct {
	path string
}

func newHostOSRebootSignalStore() hostOSRebootSignalStore {
	return hostOSRebootSignalStore{path: goalstates.ResolvedHostOSRebootSignalPath()}
}

func (s hostOSRebootSignalStore) Write(signal hostOSRebootSignal) error {
	data, err := json.Marshal(signal)
	if err != nil {
		return err
	}

	return writeFile(s.path, append(data, '\n'), 0o600)
}

// Read returns the recorded signal, or nil if there is none or it is unusable.
func (s hostOSRebootSignalStore) Read() (*hostOSRebootSignal, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, err
	}

	var signal hostOSRebootSignal
	if err := json.Unmarshal(data, &signal); err != nil {
		return nil, fmt.Errorf("decode HostOSReboot signal %s: %w", s.path, err)
	}

	signal.OperationName = strings.TrimSpace(signal.OperationName)
	signal.BootID = strings.TrimSpace(signal.BootID)

	if signal.OperationName == "" {
		slog.Warn("HostOSReboot signal missing operation name; ignoring signal", "path", s.path)
		return nil, nil
	}

	return &signal, nil
}

func (s hostOSRebootSignalStore) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	return nil
}

func readBootID() (string, error) {
	data, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot ID: %w", err)
	}

	id := strings.TrimSpace(string(data))
	if id == "" {
		return "", fmt.Errorf("read boot ID: %s is empty", bootIDPath)
	}

	return id, nil
}

// rebootHostTimeout bounds systemctl calls that run without the caller's
// cancellation.
const rebootHostTimeout = time.Minute

// rebootHost asks systemd to reboot the host. systemctl runs without the
// caller's cancellation because systemd stops this daemon, and cancels its
// context, as soon as the reboot starts. If systemctl still reports an error,
// rebootHost succeeds when systemd is already stopping, because the reboot is
// then under way.
func rebootHost(ctx context.Context, log *slog.Logger) error {
	cmdCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rebootHostTimeout)
	defer cancel()

	rebootErr := executil.RunCmd(cmdCtx, log, executil.Systemctl(), "reboot")
	if rebootErr == nil {
		return nil
	}

	state, err := executil.OutputCmd(cmdCtx, log, "systemctl", "show", "--property=SystemState", "--value")
	if err == nil && strings.TrimSpace(state) == "stopping" {
		log.Warn("systemctl reboot reported an error, but systemd is already stopping", "error", rebootErr)
		return nil
	}

	return fmt.Errorf("systemctl reboot: %w", rebootErr)
}

func (t *machineOperationTarget) reconcileHostOSReboot(ctx context.Context, store daemon.MachineOperationStore[int64], op daemon.MachineOperation) (ctrl.Result, error) {
	// The host is already going down for a reboot this daemon issued. The
	// daemon that starts after it reports that operation and picks up any
	// other that is still Pending.
	if t.rebootRequested.Load() {
		t.log.Info("not starting a HostOSReboot: this host is already rebooting", "operation", op.Name)
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

	if err := store.MarkInProgress(ctx, op, "rebooting host operating system"); err != nil {
		return ctrl.Result{}, err
	}

	machine, err := getLocalMachine(ctx, t.Client, t.machineName)
	if err != nil {
		return ctrl.Result{}, err
	}

	bootID, err := t.nodeOperator.BootID()
	if err != nil {
		return finishFailedMachineOperation(ctx, store, op, err)
	}

	signals := newHostOSRebootSignalStore()
	if err := signals.Write(hostOSRebootSignal{
		OperationName:             op.Name,
		ObservedMachineGeneration: machine.Generation,
		BootID:                    bootID,
	}); err != nil {
		return finishFailedMachineOperation(ctx, store, op, fmt.Errorf("record HostOSReboot signal: %w", err))
	}

	// The operation stays InProgress. The daemon that starts after the reboot
	// reports it from the signal.
	t.log.Info("rebooting host for HostOSReboot", "operation", op.Name, "bootID", bootID)
	t.rebootRequested.Store(true)

	if err := t.nodeOperator.RebootHost(ctx, t.log); err != nil {
		// A canceled context means this daemon is stopping, possibly because
		// the reboot has started. Keep the signal so the next daemon decides
		// from the boot ID whether the reboot happened.
		if ctx.Err() != nil {
			t.log.Warn("daemon stopping while rebooting host; the next daemon reports HostOSReboot",
				"operation", op.Name, "error", err)

			return ctrl.Result{}, nil
		}

		t.rebootRequested.Store(false)

		if clearErr := signals.Clear(); clearErr != nil {
			t.log.Warn("failed to clear HostOSReboot signal", "error", clearErr)
		}

		return finishFailedMachineOperation(ctx, store, op, err)
	}

	return ctrl.Result{}, nil
}

// publishAndClearHostOSRebootSignal reports a HostOSReboot issued by an
// earlier daemon. The operation completes if the host has booted since the
// reboot was issued and fails otherwise, so it never stays InProgress.
func publishAndClearHostOSRebootSignal(ctx context.Context, log *slog.Logger, c client.Client, currentBootID func() (string, error)) error {
	signals := newHostOSRebootSignalStore()

	signal, err := signals.Read()
	if err != nil {
		return fmt.Errorf("read HostOSReboot signal: %w", err)
	}

	if signal == nil {
		return nil
	}

	bootID, err := currentBootID()
	if err != nil {
		return err
	}

	op := daemon.MachineOperation{Name: signal.OperationName}

	result := daemon.MachineOperationResult[int64]{
		Phase:                     v1alpha3.OperationPhaseComplete,
		Reason:                    "Succeeded",
		Message:                   "HostOSReboot completed",
		ObservedMachineGeneration: signal.ObservedMachineGeneration,
	}
	if signal.BootID == "" || signal.BootID == bootID {
		result = daemon.MachineOperationResult[int64]{
			Phase:   v1alpha3.OperationPhaseFailed,
			Reason:  "RebootNotObserved",
			Message: "host did not reboot after HostOSReboot was issued",
		}
	}

	if err := daemon.FinishMachineOperation(ctx, c, op, result); err != nil {
		return err
	}

	if err := signals.Clear(); err != nil {
		return fmt.Errorf("remove HostOSReboot signal: %w", err)
	}

	log.Info("published HostOSReboot signal", "operation", signal.OperationName, "phase", result.Phase)

	return nil
}
