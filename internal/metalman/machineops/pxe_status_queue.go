// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package machineops

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

const defaultPXEStatusQueueDebounce = 250 * time.Millisecond

// PXEStatusQueue records PXE server progress without blocking request paths on
// Kubernetes status writes. Calls merge in-memory updates per Machine and a
// background worker flushes the latest debounced state to CR status.
type PXEStatusQueue struct {
	Client client.Client
	Now    func() metav1.Time
	Log    *slog.Logger

	// Debounce controls how long to wait after the last event for a Machine
	// before flushing it. If unset, a small default coalesces bursts from PXE
	// boot and first-boot cloud-init webhooks.
	Debounce time.Duration

	once      sync.Once
	workqueue workqueue.TypedRateLimitingInterface[string]

	mu      sync.Mutex
	pending map[string]*pxeStatusPending
	timers  map[string]*time.Timer
}

type pxeStatusPending struct {
	bootLoaderFilename string
	bootImageStage     string
	cloudInitStage     string
	cloudInitMessage   string
	machineCondition   *metav1.Condition
	repave             *pxeRepaveUpdate
}

type pxeRepaveUpdate struct {
	counter int64
	image   string
}

func (q *PXEStatusQueue) NeedLeaderElection() bool { return false }

func (q *PXEStatusQueue) Start(ctx context.Context) error {
	q.ensure()

	go wait.UntilWithContext(ctx, q.runWorker, time.Second)

	<-ctx.Done()
	q.shutdown()

	return nil
}

func (q *PXEStatusQueue) RecordBootLoaderDownloaded(_ context.Context, machineName, filename string) error {
	return q.enqueue(machineName, func(p *pxeStatusPending) {
		if p.bootLoaderFilename == "" {
			p.bootLoaderFilename = filename
		}
	})
}

func (q *PXEStatusQueue) RecordBootImageWrite(_ context.Context, machineName, stage string) error {
	switch stage {
	case BootImageWriteStarted, BootImageWriteFinished:
	default:
		return fmt.Errorf("unknown boot image write stage %q", stage)
	}

	return q.enqueue(machineName, func(p *pxeStatusPending) {
		p.mergeBootImageWrite(stage)
	})
}

func (q *PXEStatusQueue) RecordCloudInitStatus(_ context.Context, machineName, stage, message string) error {
	switch stage {
	case CloudInitStarted, CloudInitSucceeded, CloudInitFailed:
	default:
		return fmt.Errorf("unknown cloud-init stage %q", stage)
	}

	return q.enqueue(machineName, func(p *pxeStatusPending) {
		p.mergeCloudInitStatus(stage, message)
	})
}

func (q *PXEStatusQueue) RecordMachineCondition(_ context.Context, machineName string, condition metav1.Condition) error {
	return q.enqueue(machineName, func(p *pxeStatusPending) {
		cond := condition
		p.machineCondition = &cond
	})
}

func (q *PXEStatusQueue) RecordPXEDisabled(_ context.Context, machineName string, repaveCounter int64, imageName string) error {
	return q.enqueue(machineName, func(p *pxeStatusPending) {
		if p.repave == nil || repaveCounter >= p.repave.counter {
			p.repave = &pxeRepaveUpdate{counter: repaveCounter, image: imageName}
		}
	})
}

func (q *PXEStatusQueue) enqueue(machineName string, update func(*pxeStatusPending)) error {
	if q == nil || q.Client == nil || machineName == "" {
		return nil
	}

	q.ensure()

	q.mu.Lock()
	defer q.mu.Unlock()

	pending := q.pending[machineName]
	if pending == nil {
		pending = &pxeStatusPending{}
		q.pending[machineName] = pending
	}

	update(pending)
	q.scheduleLocked(machineName)

	return nil
}

func (q *PXEStatusQueue) ensure() {
	q.once.Do(func() {
		q.workqueue = workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "PXEStatus"},
		)
		q.pending = make(map[string]*pxeStatusPending)
		q.timers = make(map[string]*time.Timer)
	})
}

func (q *PXEStatusQueue) scheduleLocked(machineName string) {
	delay := q.debounce()
	if delay <= 0 {
		q.workqueue.Add(machineName)

		return
	}

	if timer := q.timers[machineName]; timer != nil {
		timer.Stop()
	}

	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		shouldAdd := false

		q.mu.Lock()
		if q.timers[machineName] == timer {
			delete(q.timers, machineName)
			shouldAdd = true
		}
		q.mu.Unlock()

		if shouldAdd {
			q.workqueue.Add(machineName)
		}
	})
	q.timers[machineName] = timer
}

func (q *PXEStatusQueue) debounce() time.Duration {
	if q.Debounce != 0 {
		return q.Debounce
	}

	return defaultPXEStatusQueueDebounce
}

func (q *PXEStatusQueue) shutdown() {
	if q == nil || q.workqueue == nil {
		return
	}

	q.mu.Lock()
	for machineName, timer := range q.timers {
		timer.Stop()
		delete(q.timers, machineName)
	}
	q.mu.Unlock()

	q.workqueue.ShutDown()
}

func (q *PXEStatusQueue) runWorker(ctx context.Context) {
	for q.processNextWorkItem(ctx) {
	}
}

func (q *PXEStatusQueue) processNextWorkItem(ctx context.Context) bool {
	machineName, shutdown := q.workqueue.Get()
	if shutdown {
		return false
	}

	defer q.workqueue.Done(machineName)

	pending := q.popPending(machineName)
	if pending == nil {
		q.workqueue.Forget(machineName)

		return true
	}

	if err := q.flush(ctx, machineName, pending); err != nil {
		q.restorePending(machineName, pending)
		q.log().Error("flushing PXE status update", "machine", machineName, "err", err)
		q.workqueue.AddRateLimited(machineName)

		return true
	}

	q.workqueue.Forget(machineName)

	return true
}

func (q *PXEStatusQueue) popPending(machineName string) *pxeStatusPending {
	q.mu.Lock()
	defer q.mu.Unlock()

	pending := q.pending[machineName]
	delete(q.pending, machineName)

	return pending
}

func (q *PXEStatusQueue) restorePending(machineName string, failed *pxeStatusPending) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if current := q.pending[machineName]; current != nil {
		failed.mergeNewer(current)
	}

	q.pending[machineName] = failed
}

func (q *PXEStatusQueue) flush(ctx context.Context, machineName string, pending *pxeStatusPending) error {
	if pending.bootLoaderFilename != "" {
		if err := (&BootLoaderDownloadRecorder{Client: q.Client, Now: q.Now}).RecordBootLoaderDownloaded(ctx, machineName, pending.bootLoaderFilename); err != nil {
			return err
		}
	}

	if pending.bootImageStage != "" {
		if err := (&BootImageWriteRecorder{Client: q.Client, Now: q.Now}).RecordBootImageWrite(ctx, machineName, pending.bootImageStage); err != nil {
			return err
		}
	}

	if pending.cloudInitStage != "" {
		if err := (&CloudInitStatusRecorder{Client: q.Client, Now: q.Now}).RecordCloudInitStatus(ctx, machineName, pending.cloudInitStage, pending.cloudInitMessage); err != nil {
			return err
		}
	}

	if pending.machineCondition != nil {
		if err := q.flushMachineCondition(ctx, machineName, *pending.machineCondition); err != nil {
			return err
		}
	}

	if pending.repave != nil {
		if err := q.flushPXEDisabled(ctx, machineName, pending.repave); err != nil {
			return err
		}
	}

	return nil
}

func (q *PXEStatusQueue) flushMachineCondition(ctx context.Context, machineName string, condition metav1.Condition) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := q.Client.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			return fmt.Errorf("get Machine: %w", err)
		}

		condition.ObservedGeneration = machine.Generation
		apimeta.SetStatusCondition(&machine.Status.Conditions, condition)

		return q.Client.Status().Update(ctx, &machine)
	})
}

func (q *PXEStatusQueue) flushPXEDisabled(ctx context.Context, machineName string, update *pxeRepaveUpdate) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var machine v1alpha3.Machine
		if err := q.Client.Get(ctx, client.ObjectKey{Name: machineName}, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil
			}

			return fmt.Errorf("get Machine: %w", err)
		}

		var statusRepave int64
		if machine.Status.Operations != nil {
			statusRepave = machine.Status.Operations.RepaveCounter
		}

		if update.counter <= statusRepave {
			return nil
		}

		if machine.Status.Operations == nil {
			machine.Status.Operations = &v1alpha3.OperationsStatus{}
		}

		machine.Status.Operations.RepaveCounter = update.counter
		apimeta.SetStatusCondition(&machine.Status.Conditions, metav1.Condition{
			Type:               v1alpha3.MachineConditionRepaved,
			Status:             metav1.ConditionTrue,
			Reason:             "Succeeded",
			Message:            "image=" + update.image,
			ObservedGeneration: machine.Generation,
		})

		return q.Client.Status().Update(ctx, &machine)
	})
}

func (q *PXEStatusQueue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}

	return slog.Default()
}

func (p *pxeStatusPending) mergeBootImageWrite(stage string) {
	if p.bootImageStage == BootImageWriteFinished {
		return
	}

	p.bootImageStage = stage
}

func (p *pxeStatusPending) mergeCloudInitStatus(stage, message string) {
	if p.cloudInitStage == CloudInitFailed || p.cloudInitStage == CloudInitSucceeded {
		return
	}

	p.cloudInitStage = stage
	p.cloudInitMessage = message
}

func (p *pxeStatusPending) mergeNewer(newer *pxeStatusPending) {
	if p.bootLoaderFilename == "" {
		p.bootLoaderFilename = newer.bootLoaderFilename
	}

	if newer.bootImageStage != "" {
		p.mergeBootImageWrite(newer.bootImageStage)
	}

	if newer.cloudInitStage != "" {
		p.mergeCloudInitStatus(newer.cloudInitStage, newer.cloudInitMessage)
	}

	if newer.machineCondition != nil {
		condition := *newer.machineCondition
		p.machineCondition = &condition
	}

	if newer.repave != nil && (p.repave == nil || newer.repave.counter >= p.repave.counter) {
		update := *newer.repave
		p.repave = &update
	}
}
