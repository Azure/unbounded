// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
	"github.com/Azure/unbounded/internal/metalman/netboot"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

const mediaFinalizer = "metalman.unbounded-cloud.io/boot-media"

// Match the Redfish AC recovery grace without extending it on each poll.
const mediaReadinessGrace = 10 * time.Minute

// mediaMaxBootAttempts bounds power cycles used to get the host to boot the
// virtual media.
const mediaMaxBootAttempts = 3

type stagedMediaClient interface {
	bootmedia.Client
	DeleteVirtualMedia(context.Context, bootmedia.Source) error
}

func (r *Reconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}

	return r.Client
}

func outstandingMedia(target v1alpha3.MachineOperationTargetStatus) bool {
	return target.BootMedia != nil && (!target.BootMedia.Released || mediaBootUnconfirmed(target))
}

func mediaBootUnconfirmed(target v1alpha3.MachineOperationTargetStatus) bool {
	b := target.BootMedia
	if b == nil || b.Abandoned || apimeta.IsStatusConditionTrue(target.Conditions, v1alpha3.MachineOperationConditionCloudInitDone) {
		return false
	}

	// After cleanup the media config serves halt, so only a loader that
	// already started can still be running an installer.
	return b.ImageWritten || b.LoaderStartedAt != nil ||
		(!b.Cleaned && (b.Phase == "PoweringOn" || b.Phase == "Installing" || b.Phase == "Installed"))
}

func hasOutstandingMedia(op *v1alpha3.MachineOperation) bool {
	return slices.ContainsFunc(op.Status.Targets, outstandingMedia)
}

func mediaMachine(target v1alpha3.MachineOperationTargetStatus) *v1alpha3.Machine {
	m := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: target.MachineRef}}
	b := target.BootMedia
	m.UID = b.MachineUID

	m.Generation = target.ObservedGeneration
	if b.Site != "" {
		m.Labels = map[string]string{siteLabel: b.Site}
	}

	m.Spec.Host = &v1alpha3.HostSpec{Netboot: b.Netboot.DeepCopy()}
	m.Status.Redfish = &v1alpha3.RedfishStatus{CertFingerprint: b.CertFingerprint}

	return m
}

func (r *Reconciler) mediaFinalizer(ctx context.Context, name string, add bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			return client.IgnoreNotFound(err)
		}

		if slices.Contains(op.Finalizers, mediaFinalizer) == add {
			return nil
		}

		if add {
			if !op.DeletionTimestamp.IsZero() {
				return fmt.Errorf("operation is being deleted")
			}

			op.Finalizers = append(op.Finalizers, mediaFinalizer)
		} else {
			op.Finalizers = slices.DeleteFunc(op.Finalizers, func(s string) bool { return s == mediaFinalizer })
		}

		return r.Update(ctx, &op)
	})
}

// Cleanup runs before conflict waits and terminal TTL handling.
func (r *Reconciler) reconcileMediaCleanup(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	for _, target := range op.Status.Targets {
		if !outstandingMedia(target) || target.BootMedia.Site != r.Site {
			continue
		}

		if target.BootMedia.Released {
			if !r.mediaRetentionExpired(op, target) {
				continue
			}

			b := *target.BootMedia
			b.Abandoned = true
			target.BootMedia = &b

			return true, r.saveMediaCleanup(ctx, op.Name, target)
		}

		if op.DeletionTimestamp.IsZero() && !op.Status.IsTerminal() && !isTerminalTarget(target) && !target.BootMedia.ImageWritten && !target.BootMedia.Cleaned {
			continue
		}
		// Do not mutate a cache-owned pointer while cleanup is in flight.
		b := *target.BootMedia

		target.BootMedia = &b
		switch {
		case !b.Cleaned:
			if err := r.cleanupMedia(ctx, target, false); err != nil {
				return true, err
			}

			target.BootMedia.Cleaned = true
			now := r.now()

			target.BootMedia.CleanedAt = &now
			if b.Phase == "Preparing" {
				target.BootMedia.StagedDeleted = true
			}
		case !b.StagedDeleted:
			if err := r.cleanupMedia(ctx, target, true); err != nil {
				return true, err
			}

			target.BootMedia.StagedDeleted = true
		case mediaHandoffPending(&b):
			if !r.mediaRetentionExpired(op, target) {
				// Keep ownership while the installer may still be writing or polling.
				continue
			}

			target.BootMedia.Abandoned = true
		default:
			if r.BootMedia == nil {
				return true, fmt.Errorf("virtual media provider is not configured")
			}

			if err := r.BootMedia.Release(ctx, b.ID); err != nil {
				return true, err
			}

			target.BootMedia.Released = true
		}

		return true, r.saveMediaCleanup(ctx, op.Name, target)
	}

	if slices.Contains(op.Finalizers, mediaFinalizer) && !hasOutstandingMedia(op) {
		return true, r.mediaFinalizer(ctx, op.Name, false)
	}

	if hasOutstandingMedia(op) && (op.Status.IsTerminal() || !op.DeletionTimestamp.IsZero()) {
		return true, nil
	}

	return false, nil
}

func (r *Reconciler) saveMediaCleanup(ctx context.Context, opName string, target v1alpha3.MachineOperationTargetStatus) error {
	return r.updateOperationStatus(ctx, opName, func(latest *v1alpha3.MachineOperation) {
		for i := range latest.Status.Targets {
			current := &latest.Status.Targets[i]
			if current.MachineRef == target.MachineRef && current.BootMedia != nil && current.BootMedia.ID == target.BootMedia.ID {
				current.BootMedia.Cleaned = target.BootMedia.Cleaned
				current.BootMedia.CleanedAt = target.BootMedia.CleanedAt
				current.BootMedia.StagedDeleted = target.BootMedia.StagedDeleted
				current.BootMedia.Released = target.BootMedia.Released
				current.BootMedia.Abandoned = current.BootMedia.Abandoned || target.BootMedia.Abandoned
			}
		}
	})
}

// mediaRetentionExpired reports whether a finished target has kept media
// ownership for a full install timeout after cleanup or reboot handoff
// without the installer finishing. Such an installer is treated as dead so
// the Machine does not stay locked forever.
func (r *Reconciler) mediaRetentionExpired(op *v1alpha3.MachineOperation, target v1alpha3.MachineOperationTargetStatus) bool {
	b := target.BootMedia
	if b == nil || b.CleanedAt == nil {
		return false
	}

	if !op.Status.IsTerminal() && op.DeletionTimestamp.IsZero() && !isTerminalTarget(target) {
		return false
	}

	since := b.CleanedAt.Time
	if b.RebootRequestedAt != nil && b.RebootRequestedAt.After(since) {
		since = b.RebootRequestedAt.Time
	}

	return r.now().Sub(since) >= r.mediaInstallTimeout()
}

func (r *Reconciler) cleanupMedia(ctx context.Context, target v1alpha3.MachineOperationTargetStatus, deleteStaged bool) error {
	if target.BootMedia.Phase == "Preparing" {
		// No BMC effect can precede the persisted Staging phase.
		return nil
	}

	if r.PowerClients == nil || r.BootMedia == nil {
		return fmt.Errorf("virtual media cleanup providers are not configured")
	}

	pc, err := r.PowerClients.ForMachine(ctx, mediaMachine(target))
	if err != nil {
		return err
	}

	vm, ok := pc.(stagedMediaClient)
	if !ok || !vm.SupportsVirtualMedia() {
		return fmt.Errorf("virtual media cleanup is unsupported")
	}

	b := target.BootMedia
	if deleteStaged {
		return vm.DeleteVirtualMedia(ctx, bootmedia.Source{Name: b.SourceName, Server: b.SourceServer})
	}

	if err := vm.CleanupVirtualMedia(ctx, bootmedia.Source{Name: b.SourceName, Server: b.SourceServer}); err != nil {
		return err
	}

	return nil
}

func (r *Reconciler) advanceVirtualMedia(ctx context.Context, op *v1alpha3.MachineOperation, machine *v1alpha3.Machine, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	if target.BootMedia != nil {
		b := *target.BootMedia

		target.BootMedia = &b
		if b.Cleaned {
			return r.advanceMediaInstalled(ctx, target, now)
		}

		if r.mediaProgressExpired(&b, now) {
			return failTarget(target, reasonTimedOut, "virtual media progress timed out in "+b.Phase, now)
		}

		var current v1alpha3.Machine
		if err := r.reader().Get(ctx, client.ObjectKey{Name: target.MachineRef}, &current); err != nil {
			if apierrors.IsNotFound(err) {
				return failTarget(target, reasonMachineNotFound, "Machine deleted during virtual media operation", now)
			}

			return targetChange{target: target, err: err}
		}

		if current.UID != b.MachineUID || !r.ownsMachine(&current) {
			return failTarget(target, reasonTargetNoLongerOwned, "Machine identity or ownership changed", now)
		}

		if b.Phase == "Installing" {
			target.Stage = v1alpha3.OperationStageWaitingRepave

			if b.LoaderStartedAt == nil && b.PhaseStartedAt != nil && now.Sub(b.PhaseStartedAt.Time) >= r.mediaBootTimeout() {
				// The BMC sometimes powers on without a usable virtual drive.
				// No installer can run before the loader fetches its config,
				// so a fresh power cycle is safe.
				if b.BootAttempts+1 >= mediaMaxBootAttempts {
					return failTarget(target, reasonTimedOut, "host did not boot the virtual media", now)
				}

				// Firmware 1.23.13 can keep a broken virtual drive for an image
				// name, and mounting the same name again does not fix it. Replace
				// the staged image with a new name before the next boot.
				b.BootAttempts++
				b.Phase = "Replacing"
				b.PhaseStartedAt = &now
				b.ReadinessStartedAt = nil
				target.Message = fmt.Sprintf("host did not boot the virtual media; retrying (attempt %d)", b.BootAttempts+1)

				return targetChange{target: target}
			}

			target.Message = "waiting for virtual media installer to write the boot image"
			if b.LoaderStartedAt == nil {
				target.Message = "waiting for host to boot the virtual media"
			}

			return targetChange{target: target}
		}
	}

	if r.PowerClients == nil || r.BootMedia == nil {
		return failTarget(target, reasonUnsupportedTarget, "virtual media providers are not configured", now)
	}

	pc, err := r.PowerClients.ForMachine(ctx, machine)
	if err != nil {
		return r.retryMediaTarget(target, err, now)
	}

	vm, ok := pc.(stagedMediaClient)
	if !ok || !vm.SupportsVirtualMedia() {
		return failTarget(target, reasonUnsupportedTarget, "BMC does not support virtual media", now)
	}

	if target.BootMedia == nil {
		if err := r.mediaFinalizer(ctx, op.Name, true); err != nil {
			return targetChange{target: target, err: err}
		}

		id, err := newMediaID()
		if err != nil {
			return targetChange{target: target, err: err}
		}

		target.BootMedia = &v1alpha3.MachineOperationBootMediaStatus{
			ID: id, Phase: "Preparing", MachineUID: machine.UID, Netboot: machine.Spec.Netboot().DeepCopy(),
			Site: r.Site, NodeName: nodeNameForMachine(machine), PhaseStartedAt: &now,
		}
		if machine.Status.Redfish != nil {
			target.BootMedia.CertFingerprint = machine.Status.Redfish.CertFingerprint
		}

		return targetChange{target: target}
	}

	b := target.BootMedia
	source := bootmedia.Source{Name: b.SourceName, Server: b.SourceServer}

	next := b.Phase
	switch b.Phase {
	case "Preparing":
		source, err = r.BootMedia.Prepare(ctx, machine, op, b.ID)
		if err == nil {
			if source.Name == "" || source.Server == "" {
				err = fmt.Errorf("virtual media provider returned an empty source")
			} else {
				b.SourceName, b.SourceServer = source.Name, source.Server
				next = "Staging"
			}
		}
	case "Replacing":
		// The persisted Replacing phase stops the loader config for this ID,
		// so no installer can start while the old image is removed.
		err = vm.CleanupVirtualMedia(ctx, source)
		if err == nil {
			err = vm.DeleteVirtualMedia(ctx, source)
		}

		if err == nil {
			err = r.BootMedia.Release(ctx, b.ID)
		}

		if err == nil {
			var id string

			id, err = newMediaID()
			if err == nil {
				b.ID, b.SourceName, b.SourceServer = id, "", ""
				next = "Preparing"
			}
		}
	case "Staging":
		err = vm.StageVirtualMedia(ctx, source)
		next = "PoweringOff"
	case "PoweringOff":
		var state redfish.PowerState

		state, err = pc.PowerState(ctx)
		if err == nil {
			switch state {
			case redfish.PowerOff:
				next = "Mounting"
			case redfish.PowerOn:
				err = pc.Reset(ctx, redfish.ResetForceOff)
			}
		}
	case "Mounting":
		err = vm.MountVirtualMedia(ctx, source)
		next = "SettingBoot"
	case "SettingBoot":
		err = vm.SetVirtualMediaBoot(ctx)
		next = "PoweringOn"
	case "PoweringOn":
		var state redfish.PowerState

		state, err = pc.PowerState(ctx)
		if err == nil {
			switch state {
			case redfish.PowerOn:
				next = "Installing"
			case redfish.PowerOff:
				err = pc.Reset(ctx, redfish.ResetOn)
			}
		}
	default:
		return failTarget(target, reasonExecutionFailed, "unknown virtual media phase "+b.Phase, now)
	}

	if err != nil {
		if errors.Is(err, netboot.ErrNotYetDownloaded) {
			target.Message = err.Error()
			return targetChange{target: target}
		}

		return r.retryMediaTarget(target, err, now)
	}

	if next != b.Phase {
		b.Phase = next
		b.PhaseStartedAt = &now
		b.ReadinessStartedAt = nil
	}

	target.Message = "virtual media: " + b.Phase

	return targetChange{target: target}
}

// newMediaID returns 128 random bits as an unguessable capability. The
// ub-<32 lowercase hex>.iso name stays below Rack SCM's 40-character limit.
func newMediaID() (string, error) {
	var capability [16]byte
	if _, err := rand.Read(capability[:]); err != nil {
		return "", fmt.Errorf("generate media capability: %w", err)
	}

	return hex.EncodeToString(capability[:]), nil
}

func (r *Reconciler) mediaProgressExpired(b *v1alpha3.MachineOperationBootMediaStatus, now metav1.Time) bool {
	if b.PhaseStartedAt == nil {
		return false
	}

	deadline := b.PhaseStartedAt.Add(r.powerActionTimeout())
	if b.Phase == "Installing" {
		deadline = b.PhaseStartedAt.Add(r.mediaInstallTimeout())
	}

	if b.ReadinessStartedAt != nil {
		if recoveryDeadline := b.ReadinessStartedAt.Add(mediaReadinessGrace); recoveryDeadline.After(deadline) {
			deadline = recoveryDeadline
		}
	}

	return !now.Time.Before(deadline)
}

func (r *Reconciler) retryMediaTarget(target v1alpha3.MachineOperationTargetStatus, err error, now metav1.Time) targetChange {
	if errors.Is(err, redfish.ErrNotReady) {
		if target.BootMedia != nil {
			if target.BootMedia.ReadinessStartedAt == nil {
				target.BootMedia.ReadinessStartedAt = &now
			}
		} else if target.StartedAt != nil && now.Sub(target.StartedAt.Time) >= mediaReadinessGrace {
			return failTarget(target, reasonTimedOut, "virtual media readiness timed out", now)
		}
	}

	return retryTarget(target, err, now, r.maxAttempts())
}

func (r *Reconciler) advanceMediaInstalled(ctx context.Context, target v1alpha3.MachineOperationTargetStatus, now metav1.Time) targetChange {
	if target.BootMedia.RebootRequestedAt == nil {
		if target.BootMedia.CleanedAt != nil && now.Sub(target.BootMedia.CleanedAt.Time) >= r.mediaInstallTimeout() {
			return failTarget(target, reasonTimedOut, "installer reboot handoff timed out; retaining media ownership", now)
		}

		target.Message = "waiting for installer reboot handoff"

		return targetChange{target: target}
	}

	if target.BootMedia.Phase != "Installed" {
		target.BootMedia.Phase = "Installed"
		target.BootMedia.PhaseStartedAt = target.BootMedia.RebootRequestedAt
	}

	// Virtual media callbacks are per target, never operation-wide.
	scoped := &v1alpha3.MachineOperation{Status: v1alpha3.MachineOperationStatus{Conditions: target.Conditions}}
	if change, done := cloudInitReplaceStatus(scoped, target, now); done {
		if target.BootMedia.PhaseStartedAt != nil && now.Sub(target.BootMedia.PhaseStartedAt.Time) >= cloudInitTimeout {
			return failTarget(target, reasonTimedOut, "waiting for cloud-init timed out", now)
		}

		return change
	}

	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: target.BootMedia.NodeName}, &node); err != nil {
		if !apierrors.IsNotFound(err) {
			return targetChange{target: target, err: err}
		}

		if target.BootMedia.PhaseStartedAt != nil && now.Sub(target.BootMedia.PhaseStartedAt.Time) >= cloudInitTimeout {
			return failTarget(target, reasonTimedOut, "waiting for Node timed out", now)
		}

		target.Stage = v1alpha3.OperationStageWaitingNode
		target.Message = "waiting for Node " + target.BootMedia.NodeName

		return targetChange{target: target}
	}

	return completeTarget(target, "HostReplace completed", now)
}

func mergeMediaCallbacks(updated *v1alpha3.MachineOperationTargetStatus, latest v1alpha3.MachineOperationTargetStatus) {
	if latest.BootMedia == nil || updated.BootMedia == nil || latest.BootMedia.ID != updated.BootMedia.ID {
		return
	}

	updated.BootMedia.ImageWritten = updated.BootMedia.ImageWritten || latest.BootMedia.ImageWritten
	updated.BootMedia.Cleaned = updated.BootMedia.Cleaned || latest.BootMedia.Cleaned

	updated.BootMedia.StagedDeleted = updated.BootMedia.StagedDeleted || latest.BootMedia.StagedDeleted
	if latest.BootMedia.RebootRequestedAt != nil {
		updated.BootMedia.RebootRequestedAt = latest.BootMedia.RebootRequestedAt.DeepCopy()
	}

	updated.BootMedia.Released = updated.BootMedia.Released || latest.BootMedia.Released
	updated.BootMedia.Abandoned = updated.BootMedia.Abandoned || latest.BootMedia.Abandoned

	if latest.BootMedia.LoaderStartedAt != nil && updated.BootMedia.LoaderStartedAt == nil {
		updated.BootMedia.LoaderStartedAt = latest.BootMedia.LoaderStartedAt.DeepCopy()
		if updated.BootMedia.BootAttempts != latest.BootMedia.BootAttempts {
			// A boot retry was decided before the loader start was seen. The
			// installer is running, so keep waiting for it instead.
			updated.BootMedia.BootAttempts = latest.BootMedia.BootAttempts
			updated.BootMedia.Phase = latest.BootMedia.Phase
			updated.BootMedia.PhaseStartedAt = latest.BootMedia.PhaseStartedAt.DeepCopy()
			updated.BootMedia.ReadinessStartedAt = latest.BootMedia.ReadinessStartedAt.DeepCopy()
			updated.Message = "waiting for virtual media installer to write the boot image"
		}
	}

	for _, cond := range latest.Conditions {
		if cond.Type == v1alpha3.MachineOperationConditionCloudInitDone || cond.Type == v1alpha3.MachineOperationConditionBootImageWritten || cond.Type == v1alpha3.MachineOperationConditionBootLoaderDownloaded {
			apimeta.SetStatusCondition(&updated.Conditions, cond)
		}
	}
}

func mediaHandoffPending(b *v1alpha3.MachineOperationBootMediaStatus) bool {
	return b.RebootRequestedAt == nil && !b.Abandoned && (b.ImageWritten || b.LoaderStartedAt != nil)
}

func (r *Reconciler) mediaBootTimeout() time.Duration {
	if r.MediaBootTimeout > 0 {
		return r.MediaBootTimeout
	}

	return 10 * time.Minute
}

func (r *Reconciler) mediaInstallTimeout() time.Duration {
	if r.MediaInstallTimeout > 0 {
		return r.MediaInstallTimeout
	}

	return time.Hour
}
