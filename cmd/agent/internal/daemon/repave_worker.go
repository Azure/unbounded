// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/pkg/agent/bootstrap"
	agentdaemon "github.com/Azure/unbounded/pkg/agent/daemon"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/installstate"
	"github.com/Azure/unbounded/pkg/agent/phases/reset"
)

// repaveWorker is the single repave executor. Management reconciliation never
// waits for its downloads; the installation lock serializes host mutations.
type repaveWorker struct {
	client     client.Client
	reader     client.Reader
	log        *slog.Logger
	wake       chan struct{}
	mu         sync.Mutex
	cancel     context.CancelFunc
	store      repaveStore
	pauseUntil time.Time
	loadOwner  func() (installstate.Record, error)
	advance    func(context.Context, *slog.Logger, *repaveState) error
	verify     func(context.Context, *slog.Logger, client.Reader, *repaveState) error
}

func (w *repaveWorker) notify() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *repaveWorker) interrupt() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.cancel != nil {
		w.cancel()
	}

	w.pauseUntil = time.Now().Add(5 * time.Second)
}

func (w *repaveWorker) Start(ctx context.Context) error {
	delay := time.Duration(0)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-w.wake:
			timer.Stop()
		case <-timer.C:
		}

		w.mu.Lock()
		pause := time.Until(w.pauseUntil)
		w.mu.Unlock()

		if pause > 0 {
			delay = pause
			continue
		}

		attempt, cancel := context.WithTimeout(ctx, 10*time.Minute)

		w.mu.Lock()

		w.cancel = cancel
		if time.Now().Before(w.pauseUntil) {
			cancel()
		}
		w.mu.Unlock()
		progress, err := w.step(attempt)

		cancel()
		w.mu.Lock()
		w.cancel = nil
		w.mu.Unlock()

		if err != nil {
			w.log.Error("repave remains pending", "error", err)

			if delay < 5*time.Second {
				delay = 5 * time.Second
			} else {
				delay = min(delay*2, time.Minute)
			}
		} else if progress {
			delay = time.Second
		} else {
			delay = 30 * time.Second
		}
	}
}

func (w *repaveWorker) step(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}

	lock, err := installstate.AcquireLock()
	if err != nil {
		return false, err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			w.log.Error("release repave ownership", "error", err)
		}
	}()

	s, err := w.store.Load()
	if err != nil || s == nil {
		return false, err
	}

	loadOwner := w.loadOwner
	if loadOwner == nil {
		loadOwner = installstate.DefaultStore().Load
	}

	record, err := loadOwner()
	if err != nil && (s.InstallID != "" || !errors.Is(err, installstate.ErrNotFound)) {
		return false, err
	}

	if err == nil {
		if err := validateRepaveOwner(s, record); err != nil {
			return false, err
		}
	}

	if s.Phase != "reporting" && s.Phase != "canceled" {
		// Publish the frozen intent before potentially long-running preparation.
		// Controller construction is independent of this attempt and remains live.
		if err := w.publish(ctx, s, "InProgress", "Repave attempt starting"); err != nil {
			return false, err
		}
	}

	if err := ctx.Err(); err != nil {
		return false, err
	}

	if s.Version == 1 {
		// Old transitions cannot prove the target MCV. Do not invent provenance
		// from a newer desired reference, or trust the old weak cleanup gate.
		if s.TransitionID == "" {
			s.TransitionID, err = installstate.NewInstallID()
			if err != nil {
				return false, err
			}

			if err := w.store.Save(s); err != nil {
				return false, err
			}
		}

		if err := w.publish(ctx, s, "LegacyTransition", "Legacy transition requires explicit retry/adoption; applied configuration provenance is unknown"); err != nil {
			return false, err
		}

		return false, nil
	}

	if s.Phase == "reporting" || s.Phase == "canceled" {
		if err := w.reportCompletion(ctx, s); err != nil {
			return false, err
		}

		return true, nil
	}

	if s.RecoveryAction == "retry" && s.RecoveryOperation != "" {
		if err := agentdaemon.FinishMachineOperation(ctx, w.client, agentdaemon.MachineOperation{Name: s.RecoveryOperation},
			agentdaemon.MachineOperationResult[int64]{Phase: v1alpha3.OperationPhaseComplete, Reason: "RetryScheduled", Message: "Retry scheduled for frozen repave intent"}); err != nil {
			return false, err
		}

		s.RecoveryOperation, s.RecoveryAction = "", ""
		if err := w.store.Save(s); err != nil {
			return false, err
		}
	}

	switch s.Phase {
	case "verifying":
		verify := w.verify
		if verify == nil {
			verify = verifyRepaveTarget
		}

		err = verify(ctx, w.log, w.reader, s)
	case "canceling":
		err = cancelRepaveTarget(ctx, w.log, s)
	default:
		if s.Phase == "committed" {
			if err := w.store.SaveApplied(s); err != nil {
				return false, err
			}
		}

		advance := w.advance
		if advance == nil {
			advance = advanceRepave
		}

		err = advance(ctx, w.log, s)
	}

	if err != nil {
		// Raw command errors may include signed URLs or credentials. Keep those
		// out of Kubernetes status; phase and transition ID locate host diagnostics.
		statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()

		if statusErr := w.publish(statusCtx, s, "Blocked", "Repave attempt failed; inspect daemon logs for diagnostics"); statusErr != nil {
			w.log.Error("report blocked repave", "error", statusErr)
		}

		return false, err
	}

	if err := w.store.Save(s); err != nil {
		return false, err
	}

	if err := w.publish(ctx, s, "InProgress", "Repave transition is progressing"); err != nil {
		return true, err
	}

	return true, nil
}

func (w *repaveWorker) publish(ctx context.Context, s *repaveState, reason, message string) error {
	var machine v1alpha3.Machine
	if err := w.reader.Get(ctx, client.ObjectKey{Name: s.SourceConfig.MachineName}, &machine); err != nil {
		return err
	}

	before := machine.DeepCopy()
	if s.Phase != "reporting" && s.Phase != "canceled" {
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:   v1alpha3.MachineConditionRepavePending,
			Status: metav1.ConditionTrue, Reason: reason, Message: "Frozen repave target has not completed", ObservedGeneration: machine.Generation,
		})
	}

	status := metav1.ConditionFalse
	if reason == "Applied" {
		status = metav1.ConditionTrue
	}

	apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
		Type: "RepaveReady", Status: status,
		Reason: reason, Message: fmt.Sprintf("transition=%s phase=%s: %s", s.TransitionID, s.Phase, message), ObservedGeneration: machine.Generation,
	})

	return w.client.Status().Patch(ctx, &machine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{}))
}

func (w *repaveWorker) reportCompletion(ctx context.Context, s *repaveState) error {
	if s.Phase == "reporting" {
		if err := w.store.SaveApplied(s); err != nil {
			return err
		}

		if s.TargetRef != nil {
			if err := markAppliedConfiguration(ctx, w.client, s.TargetConfig.MachineName, s.TargetRef); err != nil {
				return err
			}
		}

		if s.TargetRef == nil {
			var machine v1alpha3.Machine
			if err := w.reader.Get(ctx, client.ObjectKey{Name: s.TargetConfig.MachineName}, &machine); err != nil {
				return err
			}

			before := machine.DeepCopy()
			machine.Status.Configuration = nil
			apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
				Type:   v1alpha3.MachineConditionRepavePending,
				Status: metav1.ConditionUnknown, Reason: "ProvenanceUnknown", Message: "Exact applied configuration reference is unknown", ObservedGeneration: machine.Generation,
			})

			if err := w.client.Status().Patch(ctx, &machine, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}

			if err := w.publish(ctx, s, "ProvenanceUnknown", "Target committed; exact applied configuration reference is unknown"); err != nil {
				return err
			}
		} else if err := w.publish(ctx, s, "Applied", "Target committed; source cleanup completed"); err != nil {
			return err
		}
	} else {
		if err := w.publish(ctx, s, "Canceled", "Unstarted target removed; source installation retained"); err != nil {
			return err
		}
	}

	if s.RecoveryOperation != "" {
		if err := agentdaemon.FinishMachineOperation(ctx, w.client, agentdaemon.MachineOperation{Name: s.RecoveryOperation},
			agentdaemon.MachineOperationResult[int64]{Phase: v1alpha3.OperationPhaseComplete, Reason: "Succeeded", Message: "Repave recovery completed"}); err != nil {
			return err
		}
	}

	if s.Phase == "canceled" && s.RecoveryAction == "reselect" {
		if s.ReselectedConfig == nil {
			return fmt.Errorf("reselection has no frozen target")
		}

		id, err := installstate.NewInstallID()
		if err != nil {
			return err
		}

		s.TransitionID, s.TargetConfig, s.TargetRef = id, *s.ReselectedConfig, s.ReselectedRef
		s.Phase, s.RecoveryAction, s.RecoveryOperation = "preparing", "", ""
		s.Downloads, s.ReselectedConfig, s.ReselectedRef = nil, nil, nil

		return w.store.Save(s)
	}

	return w.store.Remove()
}

func cancelRepaveTarget(ctx context.Context, log *slog.Logger, s *repaveState) error {
	if err := reset.CleanupMachine(log, s.Target).Do(ctx); err != nil {
		return err
	}

	for _, path := range []string{goalstates.AppliedConfigPath(s.Target), goalstates.AppliedConfigChecksumPath(s.Target)} {
		if err := removeOwnedFile(path); err != nil {
			return err
		}
	}

	if err := bootstrap.SyncFilesystems("/var/lib/machines", goalstates.AgentConfigDir, goalstates.SystemdSystemDir, goalstates.SystemdNSpawnDir); err != nil {
		return err
	}

	s.Phase = "canceled"

	return nil
}
