// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func gantryDaemonSet(gen, observed int64, desired, updated, ready int32) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       gantryDaemonSetName,
			Namespace:  gantryNamespace,
			Generation: gen,
		},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     observed,
			DesiredNumberScheduled: desired,
			UpdatedNumberScheduled: updated,
			NumberReady:            ready,
		},
	}
}

func TestDaemonSetRolledOut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ds   *appsv1.DaemonSet
		want bool
	}{
		{
			name: "fully rolled out",
			ds:   gantryDaemonSet(1, 1, 3, 3, 3),
			want: true,
		},
		{
			name: "stale observed generation",
			ds:   gantryDaemonSet(2, 1, 3, 3, 3),
			want: false,
		},
		{
			name: "desired is zero",
			ds:   gantryDaemonSet(1, 1, 0, 0, 0),
			want: false,
		},
		{
			name: "not all updated",
			ds:   gantryDaemonSet(1, 1, 3, 2, 2),
			want: false,
		},
		{
			name: "not all ready",
			ds:   gantryDaemonSet(1, 1, 3, 3, 2),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.Equal(t, tt.want, daemonSetRolledOut(tt.ds))
		})
	}
}

func TestWaitForDaemonSetRollout_Ready(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientset(gantryDaemonSet(1, 1, 2, 2, 2))
	inst := &installGantry{
		kubeComponentInstaller: &kubeComponentInstaller{
			kubeCli:   cli,
			logger:    discardLogger(),
			namespace: gantryNamespace,
		},
		daemonSetName: gantryDaemonSetName,
	}

	require.NoError(t, inst.waitForDaemonSetRollout(context.Background()))
}

func TestWaitForDaemonSetRollout_TimesOutWhenNotReady(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientset(gantryDaemonSet(1, 1, 3, 3, 1))
	inst := &installGantry{
		kubeComponentInstaller: &kubeComponentInstaller{
			kubeCli:      cli,
			logger:       discardLogger(),
			namespace:    gantryNamespace,
			waitTimeout:  200 * time.Millisecond,
			pollInterval: 50 * time.Millisecond,
		},
		daemonSetName: gantryDaemonSetName,
	}

	err := inst.waitForDaemonSetRollout(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out")
}

func TestWaitForDaemonSetRollout_MissingDaemonSet(t *testing.T) {
	t.Parallel()

	cli := fake.NewClientset()
	inst := &installGantry{
		kubeComponentInstaller: &kubeComponentInstaller{
			kubeCli:      cli,
			logger:       discardLogger(),
			namespace:    gantryNamespace,
			waitTimeout:  200 * time.Millisecond,
			pollInterval: 50 * time.Millisecond,
		},
		daemonSetName: gantryDaemonSetName,
	}

	err := inst.waitForDaemonSetRollout(context.Background())
	require.Error(t, err)
}

func TestValidateGantryManifestsAcceptsDefaultNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "daemonset.yaml"), []byte(gantryManifestDaemonSet(gantryNamespace)), 0o644))

	require.NoError(t, validateGantryManifests(dir))
}

func TestValidateGantryManifestsRejectsNonDefaultNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "daemonset.yaml"), []byte(gantryManifestDaemonSet("custom-gantry")), 0o644))

	err := validateGantryManifests(dir)
	require.Error(t, err)
	require.Contains(t, err.Error(), gantryNamespace)
	require.Contains(t, err.Error(), "custom-gantry")
}

func gantryManifestDaemonSet(namespace string) string {
	return `apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: gantry
  namespace: ` + namespace + `
`
}
