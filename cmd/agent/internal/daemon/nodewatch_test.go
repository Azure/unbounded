// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func nodeScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(v1alpha3.AddToScheme(s))

	return s
}

// ---------------------------------------------------------------------------
// watchNodeCR
// ---------------------------------------------------------------------------

func Test_watchNodeCR_EnqueuesOnDelete(t *testing.T) {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "test-node"},
	}

	c := fake.NewClientBuilder().
		WithScheme(nodeScheme()).
		WithObjects(node).
		Build()

	queue := newActionQueue()
	defer queue.ShutDown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Run watchNodeCR in a goroutine. It will block watching.
	errCh := make(chan error, 1)
	go func() {
		errCh <- watchNodeCR(ctx, slog.Default(), c, "test-node", queue)
	}()

	// Give the watcher time to establish.
	time.Sleep(100 * time.Millisecond)

	// Delete the Node - the fake client should fire a DELETED event.
	err := c.Delete(ctx, node)
	require.NoError(t, err)

	// Wait for the action to appear in the queue.
	actionCh := make(chan Action, 1)
	go func() {
		a, shutdown := queue.Get()
		if !shutdown {
			queue.Done(a)
			actionCh <- a
		}
	}()

	select {
	case a := <-actionCh:
		assert.Equal(t, ActionNodeDeleted, a.Type)
		assert.Equal(t, "test-node", a.Source)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for ActionNodeDeleted")
	}
}

func Test_watchNodeWithHostname_HostnameError(t *testing.T) {
	c := fake.NewClientBuilder().
		WithScheme(nodeScheme()).
		Build()

	queue := newActionQueue()
	defer queue.ShutDown()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	// Should return immediately without panic when hostname fails.
	watchNodeWithHostname(ctx, slog.Default(), c, queue, func() (string, error) {
		return "", assert.AnError
	})
}
