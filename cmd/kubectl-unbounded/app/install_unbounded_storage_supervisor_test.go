// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package app

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestNewInstallUnboundedStorageSupervisor(t *testing.T) {
	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()
	httpCli := &http.Client{Timeout: 30 * time.Second}

	inst := newInstallUnboundedStorageSupervisor("https://example.com/storage.tar.gz", httpCli, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, inst)
	require.NotNil(t, inst.kubeComponentInstaller)
	require.Equal(t, "https://example.com/storage.tar.gz", inst.fileOrURL)
	require.Equal(t, httpCli, inst.httpClient)
	require.Equal(t, logger, inst.logger)
	require.Equal(t, kubeResourcesCli, inst.kubeResourcesCli)
	require.Equal(t, kubeCli, inst.kubeCli)
	require.Equal(t, unboundedStorageSupervisorNamespace, inst.namespace)
	require.Equal(t, unboundedStorageSupervisorDaemonSetName, inst.controllerName)
	require.Equal(t, unboundedStorageSupervisorDaemonSetName, inst.daemonSetName)
	require.Equal(t, "unbounded-storage-supervisor", inst.tempPrefix)
	require.Equal(t, 5*time.Minute, inst.waitTimeout)
	require.Equal(t, 5*time.Second, inst.pollInterval)
	require.Nil(t, inst.embeddedFS, "embeddedFS should be nil when fileOrURL is provided")
}

func TestNewInstallUnboundedStorageSupervisor_EmbeddedFallback(t *testing.T) {
	logger := discardLogger()
	kubeResourcesCli := fakeclient.NewClientBuilder().Build()
	kubeCli := fake.NewClientset()

	inst := newInstallUnboundedStorageSupervisor("", nil, logger, kubeResourcesCli, kubeCli)

	require.NotNil(t, inst)
	require.Equal(t, "", inst.fileOrURL)
	require.NotNil(t, inst.embeddedFS, "embeddedFS should be set when fileOrURL is empty")
}

func TestDaemonSetRolledOut(t *testing.T) {
	tests := []struct {
		name       string
		daemonSet  *appsv1.DaemonSet
		wantRolled bool
	}{
		{
			name: "observed generation stale",
			daemonSet: storageSupervisorDaemonSet(2, appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 1,
				UpdatedNumberScheduled: 1,
				NumberReady:            1,
			}),
		},
		{
			name: "desired zero",
			daemonSet: storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
				ObservedGeneration: 1,
			}),
		},
		{
			name: "not all updated",
			daemonSet: storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 2,
				UpdatedNumberScheduled: 1,
				NumberReady:            2,
			}),
		},
		{
			name: "not all ready",
			daemonSet: storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 2,
				UpdatedNumberScheduled: 2,
				NumberReady:            1,
			}),
		},
		{
			name: "rolled out",
			daemonSet: storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
				ObservedGeneration:     1,
				DesiredNumberScheduled: 2,
				UpdatedNumberScheduled: 2,
				NumberReady:            2,
			}),
			wantRolled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.wantRolled, daemonSetRolledOut(tt.daemonSet))
		})
	}
}

func TestWaitForDaemonSetRollout_Ready(t *testing.T) {
	ds := storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
		ObservedGeneration:     1,
		DesiredNumberScheduled: 1,
		UpdatedNumberScheduled: 1,
		NumberReady:            1,
	})
	inst := newTestStorageSupervisorInstaller(fake.NewClientset(ds))

	require.NoError(t, inst.waitForDaemonSetRollout(context.Background()))
}

func TestWaitForDaemonSetRollout_Timeout(t *testing.T) {
	ds := storageSupervisorDaemonSet(1, appsv1.DaemonSetStatus{
		ObservedGeneration:     1,
		DesiredNumberScheduled: 1,
		UpdatedNumberScheduled: 1,
	})
	inst := newTestStorageSupervisorInstaller(fake.NewClientset(ds))

	err := inst.waitForDaemonSetRollout(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out waiting for DaemonSet")
	require.Contains(t, err.Error(), "ready 0/1")
}

func TestWaitForDaemonSetRollout_MissingDaemonSet(t *testing.T) {
	inst := newTestStorageSupervisorInstaller(fake.NewClientset())

	err := inst.waitForDaemonSetRollout(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), "timed out waiting for DaemonSet")
}

func storageSupervisorDaemonSet(generation int64, status appsv1.DaemonSetStatus) *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       unboundedStorageSupervisorDaemonSetName,
			Namespace:  unboundedStorageSupervisorNamespace,
			Generation: generation,
		},
		Status: status,
	}
}

func newTestStorageSupervisorInstaller(kubeCli *fake.Clientset) *installUnboundedStorageSupervisor {
	inst := newInstallUnboundedStorageSupervisor("", nil, discardLogger(), fakeclient.NewClientBuilder().Build(), kubeCli)
	inst.waitTimeout = 200 * time.Millisecond
	inst.pollInterval = 50 * time.Millisecond

	return inst
}
