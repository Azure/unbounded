// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMediaFinalizerCleanupOwnership(t *testing.T) {
	tests := []struct {
		name       string
		site       string
		media      bool
		active     bool
		selector   bool
		expression bool
		targetOnly bool
		missing    bool
		wantRemove bool
	}{
		{name: "other site with released media", site: "rack-b", media: true},
		{name: "owner with released media", site: "rack-a", media: true, wantRemove: true},
		{name: "other site before ownership saved", site: "rack-b", active: true},
		{name: "owner before ownership saved", site: "rack-a", active: true},
		{name: "deleting other site before ownership saved", site: "rack-b"},
		{name: "deleting owner before ownership saved", site: "rack-a", wantRemove: true},
		{name: "deleting owner with target only", site: "rack-a", targetOnly: true, wantRemove: true},
		{name: "deleting other site with target only", site: "rack-b", targetOnly: true},
		{name: "deleting owner with selector", site: "rack-a", selector: true, missing: true, wantRemove: true},
		{name: "deleting other site with selector", site: "rack-b", selector: true, missing: true},
		{name: "deleting owner with expression", site: "rack-a", expression: true, missing: true, wantRemove: true},
		{name: "deleting unknown owner", site: "rack-a", missing: true},
		{name: "deleting owner with saved site and missing Machine", site: "rack-a", media: true, missing: true, wantRemove: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, _, machine, op := mediaFixture(t)
			op.Finalizers = []string{mediaFinalizer}
			require.NoError(t, r.Update(t.Context(), op))
			op.Status.Phase = v1alpha3.OperationPhaseInProgress

			op.Status.Targets = []v1alpha3.MachineOperationTargetStatus{{MachineRef: machine.Name}}
			if tt.media {
				op.Status.Targets[0].BootMedia = &v1alpha3.MachineOperationBootMediaStatus{Site: "rack-a", Released: true}
			}

			require.NoError(t, r.Status().Update(t.Context(), op))

			if tt.selector || tt.expression || tt.targetOnly {
				op.Spec.MachineRef = ""
				if tt.selector {
					op.Spec.MachineSelector = &metav1.LabelSelector{MatchLabels: map[string]string{siteLabel: "rack-a"}}
				}

				if tt.expression {
					op.Spec.MachineSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
						Key: siteLabel, Operator: metav1.LabelSelectorOpIn, Values: []string{"rack-a"},
					}}}
				}

				require.NoError(t, r.Update(t.Context(), op))
			}

			if tt.missing {
				require.NoError(t, r.Delete(t.Context(), machine))
			}

			if !tt.active {
				require.NoError(t, r.Delete(t.Context(), op))
			}

			r.Site = tt.site
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			_, _, err := r.reconcileMediaCleanup(t.Context(), op)
			require.NoError(t, err)

			err = r.Get(t.Context(), client.ObjectKeyFromObject(op), op)
			if tt.wantRemove {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
				require.Contains(t, op.Finalizers, mediaFinalizer)
			}
		})
	}
}

func TestMediaFinalizerRestartBeforeOwnershipSaved(t *testing.T) {
	r, d, _, op := mediaFixture(t)
	_, err := r.mediaFinalizer(t.Context(), op.Name, true)
	require.NoError(t, err)

	r = testReconciler(r.Client, d, "rack-a")
	r.BootMedia = d
	current := mediaStep(t, r, op.Name)
	require.Contains(t, current.Finalizers, mediaFinalizer)
	require.Equal(t, "Preparing", current.Status.Targets[0].BootMedia.Phase)
	require.Empty(t, d.calls)
}

func TestMediaFinalizerCleanupRechecksStatus(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "stale snapshot", true: "conflicting status write"}[conflict], func(t *testing.T) {
			r, _, _, op := mediaFixture(t)
			current := mediaStep(t, r, op.Name)
			current.Status.Phase = v1alpha3.OperationPhaseComplete
			current.Status.Targets[0].BootMedia.Released = true
			require.NoError(t, r.Status().Update(t.Context(), current))
			stale := current.DeepCopy()
			base, ok := r.Client.(client.WithWatch)
			require.True(t, ok)

			acquire := func() {
				current.Status.Targets[0].BootMedia.Released = false
				require.NoError(t, base.Status().Update(t.Context(), current))
			}

			if conflict {
				r.Client = interceptor.NewClient(base, interceptor.Funcs{
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						acquire()

						err := c.Update(ctx, obj, opts...)
						require.True(t, apierrors.IsConflict(err))

						return err
					},
				})
			} else {
				acquire()
			}

			_, _, err := r.reconcileMediaCleanup(t.Context(), stale)
			require.NoError(t, err)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(op), current))
			require.Contains(t, current.Finalizers, mediaFinalizer)
			require.True(t, hasOutstandingMedia(current))
		})
	}
}

func TestMediaFinalizerRetainsPendingAcquisition(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	current := mediaStep(t, r, op.Name)
	current.Status.Targets[0].BootMedia.Released = true
	current.Status.Targets = append(current.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: "pending"})
	require.NoError(t, r.Status().Update(t.Context(), current))

	removed, err := r.mediaFinalizer(t.Context(), op.Name, false)
	require.NoError(t, err)
	require.False(t, removed)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.Contains(t, current.Finalizers, mediaFinalizer)
}

func TestMediaFinalizerProtectionBeforeEffects(t *testing.T) {
	for _, phase := range []string{"Preparing", "Staging"} {
		for _, failure := range []string{"none", "update", "response", "deleted", "deleting", "terminal"} {
			t.Run(phase+"/"+failure, func(t *testing.T) {
				r, d, machine, op := mediaFixture(t)

				current := mediaStep(t, r, op.Name)
				if phase == "Staging" {
					current = mediaStep(t, r, op.Name)
				}

				d.calls = nil
				stale := current.DeepCopy()
				base, ok := r.Client.(client.WithWatch)
				require.True(t, ok)

				if failure == "deleting" {
					require.NoError(t, base.Delete(t.Context(), current))
				} else {
					current.Finalizers = nil
					require.NoError(t, base.Update(t.Context(), current))
				}

				if failure == "deleted" {
					require.NoError(t, base.Delete(t.Context(), current))
				}

				if failure == "terminal" {
					current.Status.Phase = v1alpha3.OperationPhaseFailed
					require.NoError(t, base.Status().Update(t.Context(), current))
				}

				if failure == "update" || failure == "response" {
					r.Client = interceptor.NewClient(base, interceptor.Funcs{
						Update: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.UpdateOption) error {
							if failure == "update" {
								return errors.New("update failed")
							}

							obj.SetFinalizers(nil)

							return nil
						},
					})
				}

				change := r.advanceVirtualMedia(t.Context(), stale, machine, stale.Status.Targets[0], r.now())
				if failure == "none" {
					require.NoError(t, change.err)
					require.Len(t, d.calls, 1)
					require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(op), current))
					require.Contains(t, current.Finalizers, mediaFinalizer)
				} else {
					require.Error(t, change.err)
					require.Empty(t, d.calls)
				}
			})
		}
	}
}

func TestMediaFinalizerDeletedOperationCannotAcquire(t *testing.T) {
	r, _, _, op := mediaFixture(t)
	require.NoError(t, r.Delete(t.Context(), op))
	_, err := r.mediaFinalizer(t.Context(), op.Name, true)
	require.True(t, apierrors.IsNotFound(err))

	_, err = r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(op)})
	require.NoError(t, err)
}

func TestMediaFinalizerAcquisitionRetriesRemovalConflict(t *testing.T) {
	r, d, machine, op := mediaFixture(t)
	mediaStep(t, r, op.Name)
	current := mediaStep(t, r, op.Name)
	d.calls = nil
	base, ok := r.Client.(client.WithWatch)
	require.True(t, ok)

	updates := 0
	r.Client = interceptor.NewClient(base, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			updates++
			if updates == 1 {
				removed := current.DeepCopy()
				removed.Finalizers = nil
				require.NoError(t, c.Update(ctx, removed))
			}

			err := c.Update(ctx, obj, opts...)
			if updates == 1 {
				require.True(t, apierrors.IsConflict(err))
			}

			return err
		},
	})

	change := r.advanceVirtualMedia(t.Context(), current, machine, current.Status.Targets[0], r.now())
	require.NoError(t, change.err)
	require.Equal(t, 2, updates)
	require.Equal(t, []string{"stage"}, d.calls)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(op), current))
	require.Contains(t, current.Finalizers, mediaFinalizer)
}
