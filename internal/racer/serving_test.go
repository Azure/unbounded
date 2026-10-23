// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	ctrl "sigs.k8s.io/controller-runtime"
)

// lostLeaderManager returns like controller-runtime after unintended
// leadership loss: immediately, without joining non-leader runnables.
type lostLeaderManager struct {
	ctrl.Manager
	started chan struct{}
}

func (m lostLeaderManager) Start(context.Context) error {
	close(m.started)
	return errors.New("leader election lost")
}

func TestApplicationStartJoinsServerAfterLeadershipLoss(t *testing.T) {
	app := &Application{}
	release := make(chan struct{})

	// Model an HTTPS server still closing its listener after Start returns.
	app.servingActive.Add(1)

	go func() {
		<-release
		app.servingActive.Done()
	}()

	mgr := lostLeaderManager{started: make(chan struct{})}
	done := make(chan error, 1)

	go func() { done <- app.Start(t.Context(), mgr) }()

	<-mgr.started

	select {
	case err := <-done:
		t.Fatalf("Start returned before the HTTPS server stopped: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)

	select {
	case err := <-done:
		require.ErrorContains(t, err, "leader election lost")
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the HTTPS server stopped")
	}

	// A runnable started after shutdown must not open a listener.
	require.NoError(t, servingRunnable{app: app}.Start(t.Context()))
}

func TestServingRunnableDoesNotNeedLeaderElection(t *testing.T) {
	require.False(t, servingRunnable{}.NeedLeaderElection())
}
