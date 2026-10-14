// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
	"github.com/Azure/unbounded/internal/metalman/netboot"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

type mediaTestDriver struct {
	PowerClient
	c         client.Client
	opName    string
	calls     []string
	err       error
	supported bool
	state     redfish.PowerState
	seen      *v1alpha3.Machine
}

func (d *mediaTestDriver) ForMachine(_ context.Context, m *v1alpha3.Machine) (PowerClient, error) {
	d.seen = m.DeepCopy()
	return d, nil
}

func (d *mediaTestDriver) effect(ctx context.Context, action string) error {
	var op v1alpha3.MachineOperation
	if err := d.c.Get(ctx, client.ObjectKey{Name: d.opName}, &op); err != nil {
		return err
	}

	if len(op.Finalizers) == 0 || len(op.Status.Targets) == 0 || op.Status.Targets[0].BootMedia == nil {
		return errors.New("effect before durable ownership")
	}

	d.calls = append(d.calls, action)

	return d.err
}

func (d *mediaTestDriver) SupportsVirtualMedia() bool { return d.supported }
func (d *mediaTestDriver) StageVirtualMedia(ctx context.Context, _ bootmedia.Source) error {
	return d.effect(ctx, "stage")
}

func (d *mediaTestDriver) MountVirtualMedia(ctx context.Context, _ bootmedia.Source) error {
	return d.effect(ctx, "mount")
}

func (d *mediaTestDriver) SetVirtualMediaBoot(ctx context.Context) error {
	return d.effect(ctx, "boot")
}

func (d *mediaTestDriver) CleanupVirtualMedia(ctx context.Context, _ bootmedia.Source) error {
	return d.effect(ctx, "cleanup")
}

func (d *mediaTestDriver) DeleteVirtualMedia(ctx context.Context, _ bootmedia.Source) error {
	return d.effect(ctx, "delete")
}

func (d *mediaTestDriver) PowerState(context.Context) (redfish.PowerState, error) {
	return d.state, nil
}

func (d *mediaTestDriver) Reset(ctx context.Context, action redfish.ResetType) error {
	if err := d.effect(ctx, string(action)); err != nil {
		return err
	}

	if action == redfish.ResetForceOff {
		d.state = redfish.PowerOff
	} else {
		d.state = redfish.PowerOn
	}

	return nil
}

func (d *mediaTestDriver) Prepare(ctx context.Context, _ *v1alpha3.Machine, _ *v1alpha3.MachineOperation, id string) (bootmedia.Source, error) {
	return bootmedia.Source{Name: "ub-" + id + ".iso", Server: "10.0.0.1"}, d.effect(ctx, "prepare")
}

func (d *mediaTestDriver) Release(ctx context.Context, _ string) error {
	var op v1alpha3.MachineOperation
	if err := d.c.Get(ctx, client.ObjectKey{Name: d.opName}, &op); err != nil {
		return err
	}

	if b := op.Status.Targets[0].BootMedia; !b.Cleaned && b.Phase != "Replacing" {
		return errors.New("release before Cleaned or Replacing persisted")
	}

	return d.effect(ctx, "release")
}

func mediaFixture(t *testing.T) (*Reconciler, *mediaTestDriver, *v1alpha3.Machine, *v1alpha3.MachineOperation) {
	t.Helper()

	m := testBareMetalMachine("media-machine", "rack-a")
	m.UID = "machine-uid"
	m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolVirtualMedia
	op := testOperation("media-op", v1alpha3.OperationHostReplace)
	op.UID = "op-uid"
	op.Spec.MachineRef = m.Name
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(m, op).WithStatusSubresource(op).Build()
	d := &mediaTestDriver{c: c, opName: op.Name, supported: true, state: redfish.PowerOn}
	r := testReconciler(c, d, "rack-a")
	r.BootMedia = d

	return r, d, m, op
}

func mediaStep(t *testing.T, r *Reconciler, name string) *v1alpha3.MachineOperation {
	t.Helper()
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name}})
	require.NoError(t, err)

	var op v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKey{Name: name}, &op))

	return &op
}

func TestVirtualMediaDurableLifecycleAndRestart(t *testing.T) {
	r, d, m, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	require.Empty(t, d.calls)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)
	require.Regexp(t, `^[0-9a-f]{32}$`, current.Status.Targets[0].BootMedia.ID)
	require.Contains(t, current.Finalizers, mediaFinalizer)
	// Recreate the reconciler at each phase, retaining only API and BMC state.
	for range 9 {
		r = testReconciler(r.Client, d, "rack-a")
		r.BootMedia = d
		current = mediaStep(t, r, op.Name)
	}

	require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)
	require.Len(t, current.Status.Targets[0].BootMedia.SourceName, 39)
	require.Equal(t, []string{"prepare", "stage", "ForceOff", "mount", "boot", "On"}, d.calls)

	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), current))
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.False(t, current.Status.Targets[0].BootMedia.Released)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Released)
	mediaStep(t, r, op.Name)
	q := &StatusQueue{Client: r.Client}
	require.NoError(t, q.flushOperationCondition(t.Context(), m.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "done"))
	require.NoError(t, r.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: m.Name}}))
	mediaStep(t, r, op.Name)
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseComplete, current.Status.Phase)
	require.Empty(t, current.Finalizers)
}

func TestVirtualMediaUnsupportedBeforePower(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	d.supported = false
	current := mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
	require.Empty(t, d.calls)
	require.Nil(t, current.Status.Targets[0].BootMedia)
}

func TestVirtualMediaNotReadyThenTimeoutCleanup(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	mediaStep(t, r, op.Name)
	mediaStep(t, r, op.Name)

	d.err = redfish.ErrNotReady
	current := mediaStep(t, r, op.Name)
	require.Zero(t, current.Status.Targets[0].Attempts)
	require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(10 * time.Minute)) }
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)

	d.err = nil
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.NotContains(t, d.calls, "ForceOff")
}

func TestVirtualMediaDeletionUsesSnapshot(t *testing.T) {
	r, d, m, op := mediaFixture(t)
	for range 4 {
		mediaStep(t, r, op.Name)
	}

	require.NoError(t, r.Delete(t.Context(), m))

	var latest v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &latest))
	require.NoError(t, r.Delete(t.Context(), &latest))
	mediaStep(t, r, op.Name)
	require.Equal(t, m.Spec.Netboot(), d.seen.Spec.Netboot())
	require.Equal(t, "fp", d.seen.Status.Redfish.CertFingerprint)
	mediaStep(t, r, op.Name)
	mediaStep(t, r, op.Name)
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.NoError(t, err)
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(op), &latest)))
}

func TestVirtualMediaTerminalConflictAndCallbackMerge(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	target := current.Status.Targets[0]
	stale := target
	b := *stale.BootMedia
	stale.BootMedia = &b
	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	current.Status.Targets[0].Conditions = []metav1.Condition{{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue, Reason: "Succeeded"}}
	other := v1alpha3.MachineOperationTargetStatus{MachineRef: "another-machine", Phase: v1alpha3.OperationPhaseInProgress}
	current.Status.Targets = append(current.Status.Targets, other)
	require.NoError(t, r.Status().Update(t.Context(), current))

	stale.Phase = v1alpha3.OperationPhaseFailed
	require.NoError(t, r.applyTargetChanges(t.Context(), op.Name, []targetChange{{target: stale}}, false))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.True(t, current.Status.Targets[0].BootMedia.ImageWritten)
	require.NotNil(t, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	require.True(t, current.Status.Targets[0].BootMedia.RebootRequestedAt.Equal(ptrTo(fixedNow())))
	require.True(t, apimeta.IsStatusConditionTrue(current.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
	require.Nil(t, current.Status.Targets[1].BootMedia)
	require.Empty(t, current.Status.Targets[1].Conditions)
	keys, err := r.operationConflictKeys(t.Context(), current)
	require.NoError(t, err)
	require.Contains(t, keys, "redfish:https://bmc.example.com")
	require.Contains(t, keys, "machine:media-machine")
}

func TestVirtualMediaMergeCallbackFields(t *testing.T) {
	cloudInit := metav1.Condition{
		Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue,
		Reason: "Succeeded", LastTransitionTime: fixedNow(),
	}
	latest := v1alpha3.MachineOperationTargetStatus{
		Message: "not callback-owned",
		BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
			ID: "a", Phase: "Installing", ImageWritten: true, LoaderStartedAt: ptrTo(fixedNow()),
			RebootRequestedAt: ptrTo(metav1.NewTime(fixedNow().Add(time.Minute))),
			Cleaned:           true, StagedDeleted: true, Released: true, Abandoned: true,
		},
		Conditions: []metav1.Condition{
			cloudInit,
			{Type: v1alpha3.MachineOperationConditionBootImageWritten, Status: metav1.ConditionTrue},
			{Type: v1alpha3.MachineOperationConditionBootLoaderDownloaded, Status: metav1.ConditionTrue},
		},
	}
	updated := v1alpha3.MachineOperationTargetStatus{
		Message: "controller message",
		BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
			ID: "a", Phase: "Installed", PhaseStartedAt: ptrTo(fixedNow()), CleanedAt: ptrTo(fixedNow()),
		},
		Conditions: []metav1.Condition{{Type: "ControllerCondition", Status: metav1.ConditionTrue}},
	}
	want := updated.DeepCopy()
	want.BootMedia.ImageWritten = true
	want.BootMedia.LoaderStartedAt = latest.BootMedia.LoaderStartedAt.DeepCopy()
	want.BootMedia.RebootRequestedAt = latest.BootMedia.RebootRequestedAt.DeepCopy()
	want.BootMedia.Cleaned = true
	want.BootMedia.StagedDeleted = true
	want.BootMedia.Released = true
	want.BootMedia.Abandoned = true
	want.Conditions = append(want.Conditions, cloudInit)

	mergeMediaCallbacks(&updated, latest)
	require.Equal(t, *want, updated)
	require.NotSame(t, latest.BootMedia.LoaderStartedAt, updated.BootMedia.LoaderStartedAt)
	require.NotSame(t, latest.BootMedia.RebootRequestedAt, updated.BootMedia.RebootRequestedAt)

	mergeMediaCallbacks(&updated, v1alpha3.MachineOperationTargetStatus{
		BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ID: "a"},
	})
	require.Equal(t, *want, updated, "missing callback values must not clear saved progress")
}

func TestVirtualMediaMergeRequiresMatchingMedia(t *testing.T) {
	for _, tt := range []struct {
		name    string
		updated *v1alpha3.MachineOperationBootMediaStatus
		latest  *v1alpha3.MachineOperationBootMediaStatus
	}{
		{name: "missing updated media", latest: &v1alpha3.MachineOperationBootMediaStatus{ID: "a", ImageWritten: true}},
		{name: "missing latest media", updated: &v1alpha3.MachineOperationBootMediaStatus{ID: "a"}},
		{name: "different IDs", updated: &v1alpha3.MachineOperationBootMediaStatus{ID: "a"}, latest: &v1alpha3.MachineOperationBootMediaStatus{ID: "b", ImageWritten: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updated := v1alpha3.MachineOperationTargetStatus{BootMedia: tt.updated}
			want := updated.DeepCopy()
			latest := v1alpha3.MachineOperationTargetStatus{
				BootMedia: tt.latest,
				Conditions: []metav1.Condition{{
					Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue,
				}},
			}
			mergeMediaCallbacks(&updated, latest)
			require.Equal(t, *want, updated)
		})
	}
}

func TestVirtualMediaCleanupFailureKeepsOwnership(t *testing.T) {
	r, d, _, op := mediaFixture(t)

	var current *v1alpha3.MachineOperation
	for range 3 {
		current = mediaStep(t, r, op.Name)
	}

	current.Status.Targets[0].BootMedia.ImageWritten = true
	require.NoError(t, r.Status().Update(t.Context(), current))

	d.err = errors.New("cleanup unavailable")
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorContains(t, err, "cleanup unavailable")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.False(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.Contains(t, current.Finalizers, mediaFinalizer)
	require.True(t, hasOutstandingMedia(current))

	d.err = nil
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
}

func TestVirtualMediaPrepareFailureHasNoBMCleanup(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	mediaStep(t, r, op.Name)

	d.err = errors.New("invalid image")

	var current *v1alpha3.MachineOperation
	for range r.maxAttempts() + 1 {
		current = mediaStep(t, r, op.Name)
	}

	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)

	d.err = nil
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	mediaStep(t, r, op.Name)
	require.NotContains(t, d.calls, "cleanup")
	require.NotContains(t, d.calls, "ForceOff")
	require.Contains(t, d.calls, "release")
}

func TestVirtualMediaCloudInitIsTargetScoped(t *testing.T) {
	r, _, m, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.Cleaned = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	current.Status.Targets = append(current.Status.Targets, v1alpha3.MachineOperationTargetStatus{
		MachineRef: "second", Phase: v1alpha3.OperationPhaseInProgress,
		BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ID: "second", ImageWritten: true, Cleaned: true},
	})
	require.NoError(t, r.Status().Update(t.Context(), current))
	q := &StatusQueue{Client: r.Client}
	require.NoError(t, q.flushOperationCondition(t.Context(), m.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "done"))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.True(t, apimeta.IsStatusConditionTrue(current.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
	require.Empty(t, current.Status.Targets[1].Conditions)
	require.False(t, apimeta.IsStatusConditionTrue(current.Status.Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
}

func TestVirtualMediaCapabilityIsRandomAndDurable(t *testing.T) {
	ids := map[string]bool{}

	for range 8 {
		// Identical public inputs must not produce the same capability.
		r, d, _, op := mediaFixture(t)
		current := mediaStep(t, r, op.Name)
		id := current.Status.Targets[0].BootMedia.ID
		bytes, err := hex.DecodeString(id)
		require.NoError(t, err)
		require.Len(t, bytes, 16)
		require.Equal(t, strings.ToLower(id), id)
		require.False(t, ids[id])
		ids[id] = true

		require.Empty(t, d.calls)
		restarted := testReconciler(r.Client, d, "rack-a")
		restarted.BootMedia = d
		current = mediaStep(t, restarted, op.Name)
		require.Equal(t, id, current.Status.Targets[0].BootMedia.ID)
		require.Equal(t, "ub-"+id+".iso", current.Status.Targets[0].BootMedia.SourceName)
	}
}

func TestVirtualMediaReleaseFailureDoesNotRepeatBMCCleanup(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	for range 3 {
		mediaStep(t, r, op.Name)
	}

	var current v1alpha3.MachineOperation
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	current.Status.Targets[0].BootMedia.ImageWritten = true
	require.NoError(t, r.Status().Update(t.Context(), &current))
	mediaStep(t, r, op.Name)
	mediaStep(t, r, op.Name)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), &current))

	d.err = errors.New("local release failed")
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorContains(t, err, "local release failed")
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.False(t, current.Status.Targets[0].BootMedia.Released)
	require.Contains(t, current.Finalizers, mediaFinalizer)

	d.err = nil
	r = testReconciler(r.Client, d, "rack-a")
	r.BootMedia = d
	mediaStep(t, r, op.Name)
	require.Equal(t, []string{"prepare", "stage", "cleanup", "delete", "release", "release"}, d.calls)
}

func TestVirtualMediaPhaseDeadlineWithoutReadiness(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	mediaStep(t, r, op.Name)
	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(r.powerActionTimeout())) }
	current := mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
	require.Contains(t, current.Status.Targets[0].Message, "progress timed out")
}

func TestVirtualMediaReadinessGraceSurvivesRestart(t *testing.T) {
	for _, recover := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "recovers"}[recover], func(t *testing.T) {
			r, d, _, op := mediaFixture(t)
			mediaStep(t, r, op.Name)
			mediaStep(t, r, op.Name)

			d.err = redfish.ErrNotReady
			current := mediaStep(t, r, op.Name)
			started := current.Status.Targets[0].BootMedia.ReadinessStartedAt
			require.NotNil(t, started)

			for _, elapsed := range []time.Duration{6 * time.Minute, 9 * time.Minute} {
				r = testReconciler(r.Client, d, "rack-a")
				r.BootMedia = d
				r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(elapsed)) }
				current = mediaStep(t, r, op.Name)
				require.Equal(t, v1alpha3.OperationPhaseInProgress, current.Status.Phase)
				require.Equal(t, started, current.Status.Targets[0].BootMedia.ReadinessStartedAt)
				require.Zero(t, current.Status.Targets[0].Attempts)
			}

			if recover {
				d.err = nil
				current = mediaStep(t, r, op.Name)
				require.Equal(t, "PoweringOff", current.Status.Targets[0].BootMedia.Phase)
				require.Nil(t, current.Status.Targets[0].BootMedia.ReadinessStartedAt)
			} else {
				r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(mediaReadinessGrace)) }
				current = mediaStep(t, r, op.Name)
				require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
				require.Zero(t, current.Status.Targets[0].Attempts)
			}
		})
	}
}

func TestVirtualMediaRealProviderCallbackAndPartialCleanup(t *testing.T) {
	r, d, m, op := mediaFixture(t)
	cache := netboot.NewOCICache(t.TempDir())
	digest := strings.Repeat("a", 64)
	ref := "test/netboot@sha256:" + digest

	files := map[string]string{
		"metadata.yaml": "mediaFormat: unbounded-uefi-v1\nmediaShim: shimx64.efi\nmediaGrub: grubx64.efi\n",
		"shimx64.efi":   "shim", "grubx64.efi": "grub",
		"grub/grub.cfg.tmpl": "config {{.MediaCallbackURL}}",
	}
	for name, data := range files {
		path := filepath.Join(cache.CacheDir, "oci", "sha256_"+digest, "amd64", "disk", name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(data), 0o600))
	}

	cache.SetDigestForArchitecture(ref, "amd64", "sha256:"+digest)
	m.Spec.Netboot().NetbootImage = ref
	m.Spec.Netboot().Image = "test/disk@sha256:" + strings.Repeat("b", 64)
	m.Spec.Netboot().DHCPLeases = []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}}
	require.NoError(t, r.Update(t.Context(), m))
	p := &netboot.MediaProvider{Client: r.Client, CacheDir: cache.CacheDir, TFTPServer: "192.0.2.2", Resolver: &netboot.FileResolver{Cache: cache, Reader: r.Client, ServeURL: "http://192.0.2.2:8080"}}
	builds := 0
	p.Run = func(_ context.Context, command string, args ...string) error {
		if command == "xorriso" {
			builds++

			for i, arg := range args {
				if arg == "-o" {
					return os.WriteFile(args[i+1], []byte("iso"), 0o600)
				}
			}
		}

		return nil
	}
	r.BootMedia = p
	current := mediaStep(t, r, op.Name)
	id := current.Status.Targets[0].BootMedia.ID

	require.Empty(t, d.calls)
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)
	require.Equal(t, 1, builds)
	f, err := p.OpenISO(t.Context(), current.Status.Targets[0].BootMedia.SourceName)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	for range 8 {
		current = mediaStep(t, r, op.Name)
	}

	require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)

	request := func(method, action string) int {
		req := httptest.NewRequest(method, "/bootmedia/"+id+"/"+action, nil)
		req.RemoteAddr = "192.0.2.20:4567"
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)

		return w.Code
	}
	require.Equal(t, http.StatusServiceUnavailable, request(http.MethodGet, "ready"))
	require.Equal(t, http.StatusOK, request(http.MethodPost, "complete"))

	d.err = errors.New("unmount temporarily unavailable")
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.Error(t, err)
	require.Equal(t, http.StatusServiceUnavailable, request(http.MethodGet, "ready"))

	d.err = nil
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.False(t, current.Status.Targets[0].BootMedia.Released)
	require.False(t, current.Status.IsTerminal())
	require.Equal(t, http.StatusOK, request(http.MethodGet, "ready"))
	require.FileExists(t, filepath.Join(p.CacheDir, "bootmedia", id, "bootstrap.iso"))

	d.err = errors.New("rack delete temporarily unavailable")
	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.ErrorContains(t, err, "rack delete")
	require.Equal(t, http.StatusOK, request(http.MethodGet, "ready"))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.NotNil(t, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.False(t, current.Status.Targets[0].BootMedia.StagedDeleted)
	require.True(t, hasOutstandingMedia(current))

	d.err = nil
	// Restart between persisted cleanup and local file release.
	r = testReconciler(r.Client, d, "rack-a")
	r.BootMedia = p
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Released)
	require.NoDirExists(t, filepath.Join(p.CacheDir, "bootmedia", id))
	require.Equal(t, http.StatusOK, request(http.MethodGet, "ready"))
	mediaStep(t, r, op.Name)
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationStageWaitingCloudInit, current.Status.Targets[0].Stage)
	require.Equal(t, http.StatusOK, request(http.MethodGet, "ready"))

	q := &StatusQueue{Client: r.Client}
	require.NoError(t, q.flushOperationCondition(t.Context(), m.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "done"))
	require.NoError(t, r.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: m.Name}}))
	mediaStep(t, r, op.Name)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.IsTerminal())
	require.Equal(t, http.StatusOK, request(http.MethodGet, "ready"))
}

func TestVirtualMediaLongInstallAndLateHandoff(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	current := mediaInstalling(t, r, op.Name)
	current.Status.Targets[0].BootMedia.LoaderStartedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), current))

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(30 * time.Minute)) }
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, current.Status.Phase)

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Hour)) }
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)
	current = mediaStep(t, r, op.Name)
	require.False(t, current.Status.Targets[0].BootMedia.Released)
	require.Contains(t, current.Finalizers, mediaFinalizer)
	require.NotContains(t, d.calls, "release")
	// A late installer handoff must not revive the failed operation.
	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(r.now())
	require.NoError(t, r.Status().Update(t.Context(), current))
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Released)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
}

func TestVirtualMediaCloudInitDeadlineStartsAtRebootHandoff(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	target := current.Status.Targets[0]
	target.BootMedia.Cleaned = true
	target.BootMedia.CleanedAt = ptrTo(fixedNow())
	target.BootMedia.ImageWritten = true
	now := metav1.NewTime(fixedNow().Add(30 * time.Minute))
	change := r.advanceMediaInstalled(t.Context(), target, now)
	require.Equal(t, v1alpha3.OperationPhaseInProgress, change.target.Phase)
	require.Contains(t, change.target.Message, "handoff")
	target.BootMedia.RebootRequestedAt = &now
	change = r.advanceMediaInstalled(t.Context(), target, now)
	require.Equal(t, v1alpha3.OperationStageWaitingCloudInit, change.target.Stage)
	require.Equal(t, &now, change.target.BootMedia.PhaseStartedAt)
	change.target.Conditions = []metav1.Condition{{
		Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionFalse,
		Reason: "Running", LastTransitionTime: metav1.NewTime(now.Add(time.Minute)),
	}}
	change = r.advanceMediaInstalled(t.Context(), change.target, metav1.NewTime(now.Add(cloudInitTimeout-time.Second)))
	require.Equal(t, v1alpha3.OperationPhaseInProgress, change.target.Phase)
	change = r.advanceMediaInstalled(t.Context(), change.target, metav1.NewTime(now.Add(cloudInitTimeout)))
	require.Equal(t, v1alpha3.OperationPhaseFailed, change.target.Phase)
	require.Equal(t, "TimedOut: waiting for cloud-init timed out", change.target.Message)
	target.BootMedia.RebootRequestedAt = nil
	change = r.advanceMediaInstalled(t.Context(), target, metav1.NewTime(fixedNow().Add(time.Hour)))
	require.Equal(t, v1alpha3.OperationPhaseFailed, change.target.Phase)
	require.True(t, mediaHandoffPending(change.target.BootMedia))
}

func TestVirtualMediaConfiguredInstallDeadline(t *testing.T) {
	r := &Reconciler{MediaInstallTimeout: 2 * time.Hour, PowerActionTimeout: time.Minute}
	b := &v1alpha3.MachineOperationBootMediaStatus{Phase: "Installing", PhaseStartedAt: ptrTo(fixedNow())}
	require.False(t, r.mediaProgressExpired(b, metav1.NewTime(fixedNow().Add(time.Hour))))
	require.True(t, r.mediaProgressExpired(b, metav1.NewTime(fixedNow().Add(2*time.Hour))))
}

func TestVirtualMediaDeletingLostReadyResponseRetainsCallback(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleting", true: "failed-and-deleting"}[failed], func(t *testing.T) {
			r, _, m, op := mediaFixture(t)

			var current *v1alpha3.MachineOperation
			for range 10 {
				current = mediaStep(t, r, op.Name)
			}

			current.Status.Targets[0].BootMedia.Netboot.DHCPLeases = []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}}

			current.Status.Targets[0].BootMedia.ImageWritten = true
			if failed {
				current.Status.Phase = v1alpha3.OperationPhaseFailed
				current.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
			}

			require.NoError(t, r.Status().Update(t.Context(), current))
			require.NoError(t, r.Delete(t.Context(), current))
			mediaStep(t, r, op.Name)
			p := &netboot.MediaProvider{Client: r.Client}
			ready := func() int {
				req := httptest.NewRequest(http.MethodGet, "/bootmedia/"+current.Status.Targets[0].BootMedia.ID+"/ready", nil)
				req.RemoteAddr = "192.0.2.20:5678"
				w := httptest.NewRecorder()
				p.ServeHTTP(w, req)

				return w.Code
			}
			require.Equal(t, http.StatusOK, ready())
			// Treat the first success response as lost, then drain cleanup.
			for range 4 {
				current = mediaStep(t, r, op.Name)
			}

			require.True(t, current.Status.Targets[0].BootMedia.Released)
			require.Contains(t, current.Finalizers, mediaFinalizer)
			require.True(t, hasOutstandingMedia(current))
			require.Equal(t, http.StatusOK, ready())

			newer := testOperation("newer-op", v1alpha3.OperationHostReplace)
			newer.Spec.MachineRef = m.Name
			newer.CreationTimestamp = metav1.NewTime(fixedNow().Add(time.Hour))
			require.NoError(t, r.Create(t.Context(), newer))
			q := &StatusQueue{Client: r.Client}
			require.NoError(t, q.flushOperationCondition(t.Context(), m.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "booted"))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
			require.True(t, apimeta.IsStatusConditionTrue(current.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(newer), newer))
			require.Empty(t, newer.Status.Conditions)
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
			require.NoError(t, err)
			require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(op), current)))
		})
	}
}

func TestVirtualMediaKnownSuccessAfterCleanupDelay(t *testing.T) {
	r, d, m, op := mediaFixture(t)

	var current *v1alpha3.MachineOperation
	for range 10 {
		current = mediaStep(t, r, op.Name)
	}

	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), current))
	mediaStep(t, r, op.Name)

	d.err = errors.New("rack deletion unavailable")
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: op.Name}})
	require.Error(t, err)

	q := &StatusQueue{Client: r.Client}
	require.NoError(t, q.flushOperationCondition(t.Context(), m.Name, v1alpha3.MachineOperationConditionCloudInitDone, metav1.ConditionTrue, "Succeeded", "booted"))
	require.NoError(t, r.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: m.Name}}))
	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(20 * time.Minute)) }
	d.err = nil

	for range 4 {
		current = mediaStep(t, r, op.Name)
	}

	require.Equal(t, v1alpha3.OperationPhaseComplete, current.Status.Phase)
}

func TestVirtualMediaUsesFrozenActionsAndRejectsReplacement(t *testing.T) {
	r, d, m, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	frozen := current.Status.Targets[0].BootMedia.Netboot.DeepCopy()
	m.Spec.Netboot().Redfish.URL = "https://different-bmc"
	m.Spec.Netboot().Image = "changed-image"
	require.NoError(t, r.Update(t.Context(), m))
	mediaStep(t, r, op.Name)
	require.Equal(t, frozen, d.seen.Spec.Netboot())
	require.NoError(t, r.Delete(t.Context(), m))
	replacement := m.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-uid"
	require.NoError(t, r.Create(t.Context(), replacement))
	current = mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
	require.NotContains(t, d.calls, "stage")
	current = mediaStep(t, r, op.Name)
	require.True(t, current.Status.Targets[0].BootMedia.Cleaned)
	require.Equal(t, frozen, d.seen.Spec.Netboot())
}

func mediaInstalling(t *testing.T, r *Reconciler, name string) *v1alpha3.MachineOperation {
	t.Helper()

	var current *v1alpha3.MachineOperation
	for range 10 {
		current = mediaStep(t, r, name)
	}

	require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)

	return current
}

func TestVirtualMediaBootRetryWithoutLoaderStart(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	mediaInstalling(t, r, op.Name)

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(9 * time.Minute)) }
	current := mediaStep(t, r, op.Name)
	require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)
	require.Contains(t, current.Status.Targets[0].Message, "waiting for host to boot")

	// No loader start after the boot timeout: replace the image under a new
	// name, then power cycle and mount again.
	oldID := current.Status.Targets[0].BootMedia.ID
	resets := slices.Clone(d.calls)
	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(10 * time.Minute)) }
	current = mediaStep(t, r, op.Name)
	b := current.Status.Targets[0].BootMedia
	require.Equal(t, "Replacing", b.Phase)
	require.Equal(t, oldID, b.ID)
	require.Equal(t, int32(1), b.BootAttempts)
	require.Equal(t, int32(0), current.Status.Targets[0].Attempts)
	require.Contains(t, current.Status.Targets[0].Message, "attempt 2")
	require.Len(t, d.calls, len(resets), "the retry decision must be saved before side effects")

	current = mediaStep(t, r, op.Name)
	b = current.Status.Targets[0].BootMedia
	require.Equal(t, "Preparing", b.Phase)
	require.NotEqual(t, oldID, b.ID)
	require.Regexp(t, "^[0-9a-f]{32}$", b.ID)
	require.Empty(t, b.SourceName)
	require.Equal(t, []string{"cleanup", "delete", "release"}, d.calls[len(resets):])

	for range 8 {
		current = mediaStep(t, r, op.Name)
	}

	b = current.Status.Targets[0].BootMedia
	require.Equal(t, "Installing", b.Phase)
	require.Equal(t, "ub-"+b.ID+".iso", b.SourceName)
	require.Equal(t, []string{"cleanup", "delete", "release", "prepare", "stage", string(redfish.ResetForceOff), "mount", "boot", string(redfish.ResetOn)}, d.calls[len(resets):])
	require.False(t, b.Cleaned)
}

func TestVirtualMediaReplaceRetriesFailedCleanup(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	current := mediaInstalling(t, r, op.Name)
	oldID := current.Status.Targets[0].BootMedia.ID

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(10 * time.Minute)) }
	mediaStep(t, r, op.Name)

	d.err = errors.New("bmc unavailable")
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Replacing", current.Status.Targets[0].BootMedia.Phase)
	require.Equal(t, oldID, current.Status.Targets[0].BootMedia.ID)

	d.err = nil
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)
	require.NotEqual(t, oldID, current.Status.Targets[0].BootMedia.ID)
}

func TestVirtualMediaBootRetriesAreBounded(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	mediaInstalling(t, r, op.Name)

	offset := time.Duration(0)
	for attempt := 1; attempt < mediaMaxBootAttempts; attempt++ {
		offset += 10 * time.Minute
		r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(offset)) }

		var current *v1alpha3.MachineOperation
		for range 10 {
			current = mediaStep(t, r, op.Name)
		}

		require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)
		require.Equal(t, int32(attempt), current.Status.Targets[0].BootMedia.BootAttempts)
	}

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(offset + 10*time.Minute)) }
	current := mediaStep(t, r, op.Name)
	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Targets[0].Phase)
	require.Contains(t, current.Status.Targets[0].Message, "did not boot the virtual media")
}

func TestVirtualMediaNoBootRetryAfterLoaderStart(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	current := mediaInstalling(t, r, op.Name)
	current.Status.Targets[0].BootMedia.LoaderStartedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), current))

	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(30 * time.Minute)) }
	current = mediaStep(t, r, op.Name)
	require.Equal(t, "Installing", current.Status.Targets[0].BootMedia.Phase)
	require.Zero(t, current.Status.Targets[0].BootMedia.BootAttempts)
	require.Contains(t, current.Status.Targets[0].Message, "write the boot image")
}

func TestVirtualMediaMergeKeepsLoaderStartOverStaleRetry(t *testing.T) {
	started := ptrTo(fixedNow())
	latest := v1alpha3.MachineOperationTargetStatus{BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
		ID: "a", Phase: "Installing", PhaseStartedAt: ptrTo(fixedNow()), LoaderStartedAt: started,
	}}
	updated := v1alpha3.MachineOperationTargetStatus{BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
		ID: "a", Phase: "Replacing", PhaseStartedAt: ptrTo(metav1.NewTime(fixedNow().Add(time.Minute))), BootAttempts: 1,
	}}
	mergeMediaCallbacks(&updated, latest)
	require.Equal(t, "Installing", updated.BootMedia.Phase)
	require.Zero(t, updated.BootMedia.BootAttempts)
	require.Equal(t, started, updated.BootMedia.LoaderStartedAt)
	require.Equal(t, latest.BootMedia.PhaseStartedAt, updated.BootMedia.PhaseStartedAt)
	require.Equal(t, latest.BootMedia.ReadinessStartedAt, updated.BootMedia.ReadinessStartedAt)
	require.Contains(t, updated.Message, "write the boot image")

	// Without a retry decision, only the loader start is merged.
	updated = v1alpha3.MachineOperationTargetStatus{BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ID: "a", Phase: "Installing"}}
	mergeMediaCallbacks(&updated, latest)
	require.Equal(t, started, updated.BootMedia.LoaderStartedAt)
}

func TestVirtualMediaFailedBootReleasesWithoutHandoff(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	mediaInstalling(t, r, op.Name)

	// The host never booted the media, so no installer can be running.
	r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(2 * time.Hour)) }

	var current *v1alpha3.MachineOperation
	for range 5 {
		current = mediaStep(t, r, op.Name)
	}

	require.True(t, current.Status.Targets[0].BootMedia.Released)
	require.Contains(t, d.calls, "release")
	require.False(t, hasOutstandingMedia(current))
	require.NotContains(t, current.Finalizers, mediaFinalizer)
}

func TestVirtualMediaAbandonsDeadInstallerAfterTimeout(t *testing.T) {
	for _, handoff := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-handoff", true: "no-cloud-init"}[handoff], func(t *testing.T) {
			r, _, _, op := mediaFixture(t)
			current := mediaInstalling(t, r, op.Name)
			current.Status.Targets[0].BootMedia.LoaderStartedAt = ptrTo(fixedNow())
			require.NoError(t, r.Status().Update(t.Context(), current))

			r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(time.Hour)) }
			for range 4 {
				current = mediaStep(t, r, op.Name)
			}

			require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
			require.True(t, current.Status.Targets[0].BootMedia.StagedDeleted)

			if handoff {
				current.Status.Targets[0].BootMedia.ImageWritten = true
				current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(metav1.NewTime(fixedNow().Add(time.Hour)))
				require.NoError(t, r.Status().Update(t.Context(), current))
			}

			current = mediaStep(t, r, op.Name)
			require.True(t, hasOutstandingMedia(current))
			require.Contains(t, current.Finalizers, mediaFinalizer)
			require.False(t, current.Status.Targets[0].BootMedia.Abandoned)

			r.Now = func() metav1.Time { return metav1.NewTime(fixedNow().Add(2*time.Hour + time.Minute)) }
			for range 4 {
				current = mediaStep(t, r, op.Name)
			}

			require.True(t, current.Status.Targets[0].BootMedia.Abandoned)
			require.True(t, current.Status.Targets[0].BootMedia.Released)
			require.False(t, hasOutstandingMedia(current))
			require.NotContains(t, current.Finalizers, mediaFinalizer)
		})
	}
}

func TestVirtualMediaReleasesAfterCloudInitFailure(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	current := mediaInstalling(t, r, op.Name)
	current.Status.Targets[0].BootMedia.LoaderStartedAt = ptrTo(fixedNow())
	current.Status.Targets[0].BootMedia.ImageWritten = true
	current.Status.Targets[0].BootMedia.RebootRequestedAt = ptrTo(fixedNow())
	require.NoError(t, r.Status().Update(t.Context(), current))

	require.True(t, mediaBootUnconfirmed(current.Status.Targets[0]))

	current.Status.Targets[0].Conditions = []metav1.Condition{{
		Type:               v1alpha3.MachineOperationConditionCloudInitDone,
		Status:             metav1.ConditionFalse,
		Reason:             "Failed",
		Message:            "stage failed",
		LastTransitionTime: fixedNow(),
	}}
	require.NoError(t, r.Status().Update(t.Context(), current))

	for range 6 {
		current = mediaStep(t, r, op.Name)
	}

	require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
	require.True(t, current.Status.Targets[0].BootMedia.Released)
	require.False(t, current.Status.Targets[0].BootMedia.Abandoned)
	require.False(t, hasOutstandingMedia(current))
	require.NotContains(t, current.Finalizers, mediaFinalizer)
}

func TestCloudInitReported(t *testing.T) {
	tests := []struct {
		name string
		cond *metav1.Condition
		want bool
	}{
		{name: "missing", want: false},
		{name: "pending", cond: &metav1.Condition{Status: metav1.ConditionUnknown, Reason: "Pending"}, want: false},
		{name: "in progress", cond: &metav1.Condition{Status: metav1.ConditionFalse, Reason: "InProgress"}, want: false},
		{name: "failed", cond: &metav1.Condition{Status: metav1.ConditionFalse, Reason: "Failed"}, want: true},
		{name: "succeeded", cond: &metav1.Condition{Status: metav1.ConditionTrue, Reason: "Succeeded"}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target := v1alpha3.MachineOperationTargetStatus{}

			if tt.cond != nil {
				c := *tt.cond
				c.Type = v1alpha3.MachineOperationConditionCloudInitDone
				target.Conditions = []metav1.Condition{c}
			}

			require.Equal(t, tt.want, cloudInitReported(target))
		})
	}
}
