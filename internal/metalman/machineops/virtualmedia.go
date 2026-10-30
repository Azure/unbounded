// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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

const mediaNoLoaderTimeoutMessage = "host did not boot the virtual media"

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
	if b == nil || b.Abandoned || cloudInitReported(target) {
		return false
	}

	// After cleanup the media config serves halt, so only a loader that
	// already started can still be running an installer.
	return b.ImageWritten || b.LoaderStartedAt != nil ||
		(!b.Cleaned && (b.Phase == "PoweringOn" || b.Phase == "Installing" || b.Phase == "Installed"))
}

// cloudInitReported reports whether the installed OS has reported a final
// cloud-init result. A failure also proves the host left the installer and
// booted from disk, so the media no longer needs to be held.
func cloudInitReported(target v1alpha3.MachineOperationTargetStatus) bool {
	cond := apimeta.FindStatusCondition(target.Conditions, v1alpha3.MachineOperationConditionCloudInitDone)

	return cond != nil && (cond.Status == metav1.ConditionTrue ||
		(cond.Status == metav1.ConditionFalse && cond.Reason == "Failed"))
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

func (r *Reconciler) mediaFinalizer(ctx context.Context, name string, add bool) (bool, error) {
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var op v1alpha3.MachineOperation
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &op); err != nil {
			if add {
				return err
			}

			return client.IgnoreNotFound(err)
		}

		if add {
			if !op.DeletionTimestamp.IsZero() || op.Status.IsTerminal() {
				return fmt.Errorf("operation is being deleted or is terminal")
			}

			if !slices.Contains(op.Finalizers, mediaFinalizer) {
				op.Finalizers = append(op.Finalizers, mediaFinalizer)
			}
		} else {
			if !slices.Contains(op.Finalizers, mediaFinalizer) || hasOutstandingMedia(&op) {
				return nil
			}

			owned, err := r.canReleaseMediaFinalizer(ctx, &op)
			if err != nil || !owned {
				return err
			}

			op.Finalizers = slices.DeleteFunc(op.Finalizers, func(s string) bool { return s == mediaFinalizer })
		}

		// Even an existing finalizer needs a version-checked write before effects.
		if err := r.Update(ctx, &op); err != nil {
			return err
		}

		if add && !slices.Contains(op.Finalizers, mediaFinalizer) {
			return fmt.Errorf("operation media finalizer was not persisted")
		}

		changed = true

		return nil
	})

	return changed, err
}

func (r *Reconciler) canReleaseMediaFinalizer(ctx context.Context, op *v1alpha3.MachineOperation) (bool, error) {
	active := !op.Status.IsTerminal() && op.DeletionTimestamp.IsZero()
	owned, hasMedia := false, false

	for _, target := range op.Status.Targets {
		if target.BootMedia != nil {
			hasMedia = true
			owned = owned || target.BootMedia.Site == r.Site
		} else if active && !isTerminalTarget(target) {
			// A target may be between adding the finalizer and saving ownership.
			return false, nil
		}
	}

	if hasMedia {
		return owned, nil
	}

	if active {
		return false, nil
	}

	// Before BootMedia is saved, targets have no site. Use the selector or
	// fresh Machine labels. Missing Machines do not prove ownership.
	if op.Spec.MachineSelector != nil {
		return r.validateSelectorScope(op.Spec.MachineSelector) == nil, nil
	}

	names := []string{op.Spec.MachineRef}
	for _, target := range op.Status.Targets {
		names = append(names, target.MachineRef)
	}

	for _, name := range names {
		if name == "" {
			continue
		}

		var machine v1alpha3.Machine
		if err := r.reader().Get(ctx, client.ObjectKey{Name: name}, &machine); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}

			return false, err
		}

		if r.ownsMachine(&machine) {
			return true, nil
		}
	}

	return false, nil
}

// Cleanup runs before conflict waits and terminal TTL handling.
func (r *Reconciler) reconcileMediaCleanup(ctx context.Context, op *v1alpha3.MachineOperation) (bool, map[string]struct{}, error) {
	failed := map[string]struct{}{}

	var (
		cleanupErr error
		changed    bool
	)

	for i, target := range op.Status.Targets {
		if !outstandingMedia(target) || target.BootMedia.Site != r.Site {
			continue
		}

		// Keep failed or unsaved cleanup out of the advancement snapshot.
		target = *target.DeepCopy()

		updated, err := r.cleanupMediaTarget(ctx, op, &target)
		if err == nil && updated {
			err = r.saveMediaCleanup(ctx, op.Name, target)
		}

		if err != nil {
			slog.ErrorContext(ctx, "virtual media cleanup failed", "machineOperation", op.Name, "machine", target.MachineRef, "error", err)
			failed[target.MachineRef] = struct{}{}
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("cleanup virtual media for Machine %s: %w", target.MachineRef, err))

			continue
		}

		if updated {
			op.Status.Targets[i] = target
			changed = true
		}
	}

	if !changed && slices.Contains(op.Finalizers, mediaFinalizer) && !hasOutstandingMedia(op) {
		removed, err := r.mediaFinalizer(ctx, op.Name, false)
		if removed || err != nil {
			return true, failed, errors.Join(cleanupErr, err)
		}
	}

	if (changed || hasOutstandingMedia(op)) && (op.Status.IsTerminal() || !op.DeletionTimestamp.IsZero()) {
		return true, failed, cleanupErr
	}

	// Preserve the cleanup-only reconcile for single-target operations.
	return len(op.Status.Targets) == 1 && (changed || cleanupErr != nil), failed, cleanupErr
}

func (r *Reconciler) cleanupMediaTarget(ctx context.Context, op *v1alpha3.MachineOperation, target *v1alpha3.MachineOperationTargetStatus) (bool, error) {
	b := target.BootMedia
	if b.Released {
		if !r.mediaRetentionExpired(op, *target) {
			return false, nil
		}

		b.Abandoned = true

		return true, nil
	}

	if op.DeletionTimestamp.IsZero() && !op.Status.IsTerminal() && !isTerminalTarget(*target) && !b.ImageWritten && !b.Cleaned {
		return false, nil
	}

	switch {
	case !b.Cleaned:
		if err := r.cleanupMedia(ctx, *target, false); err != nil {
			return false, err
		}

		b.Cleaned = true
		now := r.now()

		b.CleanedAt = &now
		if b.Phase == "Preparing" {
			b.StagedDeleted = true
		}
	case !b.StagedDeleted:
		if err := r.cleanupMedia(ctx, *target, true); err != nil {
			return false, err
		}

		b.StagedDeleted = true
	case mediaHandoffPending(b):
		if !r.mediaRetentionExpired(op, *target) {
			// Keep ownership while the installer may still be writing or polling.
			return false, nil
		}

		b.Abandoned = true
	default:
		if r.BootMedia == nil {
			return false, fmt.Errorf("virtual media provider is not configured")
		}

		if err := r.BootMedia.Release(ctx, b.ID); err != nil {
			return false, err
		}

		b.Released = true
	}

	return true, nil
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

	vm, ok := pc.(bootmedia.Client)
	if !ok || !vm.SupportsVirtualMedia() {
		return fmt.Errorf("virtual media cleanup is unsupported")
	}

	b := target.BootMedia
	if deleteStaged {
		return vm.DeleteVirtualMedia(ctx, bootmedia.Source{Name: b.SourceName, Server: b.SourceServer})
	}

	return vm.CleanupVirtualMedia(ctx, bootmedia.Source{Name: b.SourceName, Server: b.SourceServer})
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
				return retryVirtualMediaBoot(target, &b, now)
			}

			if b.LoaderStartedAt != nil && !b.ImageWritten && b.RebootRequestedAt == nil && r.PowerClients != nil {
				pc, err := r.PowerClients.ForMachine(ctx, mediaMachine(target))
				if err != nil {
					target.Message = "waiting for virtual media installer to write the boot image; host power state unavailable: " + err.Error()

					return targetChange{target: target}
				}

				state, err := pc.PowerState(ctx)
				if err != nil {
					target.Message = "waiting for virtual media installer to write the boot image; host power state unavailable: " + err.Error()

					return targetChange{target: target}
				}

				if state == redfish.PowerOff {
					return retryVirtualMediaBoot(target, &b, now)
				}
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

	vm, ok := pc.(bootmedia.Client)
	if !ok || !vm.SupportsVirtualMedia() {
		return failTarget(target, reasonUnsupportedTarget, "BMC does not support virtual media", now)
	}

	if _, err := r.mediaFinalizer(ctx, op.Name, true); err != nil {
		return targetChange{target: target, err: err}
	}

	if target.BootMedia == nil {
		id, err := newMediaID()
		if err != nil {
			return targetChange{target: target, err: err}
		}

		target.BootMedia = &v1alpha3.MachineOperationBootMediaStatus{
			ID: id, Phase: "Preparing", MachineUID: machine.UID, Netboot: machine.Spec.Netboot().DeepCopy(),
			Site: r.Site, NodeName: machine.NodeName(), PhaseStartedAt: &now,
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
				b.LoaderStartedAt = nil
				b.ImageWritten = false
				b.RebootRequestedAt = nil
				b.Abandoned = false
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

func retryVirtualMediaBoot(target v1alpha3.MachineOperationTargetStatus, b *v1alpha3.MachineOperationBootMediaStatus, now metav1.Time) targetChange {
	if b.BootAttempts+1 >= mediaMaxBootAttempts {
		if b.LoaderStartedAt != nil {
			// Exact PowerOff proves the installer is no longer running, so
			// terminal cleanup does not need to retain the stale handoff.
			b.Abandoned = true
		}

		return failTarget(target, reasonTimedOut, mediaNoLoaderTimeoutMessage, now)
	}

	// Some firmware can keep a broken virtual drive for an image name, and
	// mounting the same name again does not fix it. Replace the staged image
	// with a new name before the next boot.
	b.BootAttempts++
	b.Phase = "Replacing"
	b.PhaseStartedAt = &now
	b.ReadinessStartedAt = nil
	target.Message = fmt.Sprintf("host did not boot the virtual media; retrying (attempt %d)", b.BootAttempts+1)

	return targetChange{target: target}
}

// newMediaID returns 128 random bits as an unguessable capability. The
// ub-<32 lowercase hex>.iso name stays below the 40-character limit of tested rack manager firmware.
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
	if change, done := cloudInitReplaceStatus(target.Conditions, target, now); done {
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
	retryDecision := updated.BootMedia.Phase == "Replacing" &&
		updated.BootMedia.BootAttempts != latest.BootMedia.BootAttempts
	retryFailed := updated.Phase == v1alpha3.OperationPhaseFailed &&
		updated.Message == reasonTimedOut+": "+mediaNoLoaderTimeoutMessage

	if (latest.BootMedia.ImageWritten || latest.BootMedia.RebootRequestedAt != nil) && (retryDecision || retryFailed) {
		// A callback won the race with a power-off recovery decision. Preserve
		// installer progress and never replace media after the image write or
		// reboot handoff has started.
		updated.Phase = latest.Phase
		updated.Stage = latest.Stage
		updated.CompletedAt = latest.CompletedAt.DeepCopy()
		updated.Message = "waiting for virtual media installer to write the boot image"
		updated.BootMedia.Phase = latest.BootMedia.Phase
		updated.BootMedia.PhaseStartedAt = latest.BootMedia.PhaseStartedAt.DeepCopy()
		updated.BootMedia.ReadinessStartedAt = latest.BootMedia.ReadinessStartedAt.DeepCopy()
		updated.BootMedia.BootAttempts = latest.BootMedia.BootAttempts
		updated.BootMedia.Abandoned = latest.BootMedia.Abandoned
	}

	updated.BootMedia.Abandoned = updated.BootMedia.Abandoned || latest.BootMedia.Abandoned

	if latest.BootMedia.LoaderStartedAt != nil && updated.BootMedia.LoaderStartedAt == nil {
		updated.BootMedia.LoaderStartedAt = latest.BootMedia.LoaderStartedAt.DeepCopy()
		if updated.Phase == v1alpha3.OperationPhaseFailed && updated.Message == reasonTimedOut+": "+mediaNoLoaderTimeoutMessage {
			// The final boot timeout leaves BootAttempts unchanged. Undo only
			// this no-loader failure, not an install timeout or another failure.
			updated.Phase = latest.Phase
			updated.CompletedAt = latest.CompletedAt.DeepCopy()
			updated.Message = "waiting for virtual media installer to write the boot image"
		}

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

	if cond := apimeta.FindStatusCondition(latest.Conditions, v1alpha3.MachineOperationConditionCloudInitDone); cond != nil {
		apimeta.SetStatusCondition(&updated.Conditions, *cond)
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
