// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"testing/synctest"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

func TestDetailCompletionDoesNotCancelDispatchWrite(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "success"
		if failure {
			name = "failure"
		}

		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				started := make(chan context.Context, 1)
				release := make(chan struct{})
				manager := testDetailRequests(t, nodeDetailRequestHooks{
					Dispatch: func(ctx context.Context, _ string, _ statusv1alpha1.DetailRequest) (bool, error) {
						started <- ctx

						select {
						case <-release:
						case <-ctx.Done():
						}

						return true, ctx.Err()
					},
				})
				request := manager.Request("node", true)
				dispatchCtx := <-started

				var err error
				if failure {
					err = manager.CompleteFailure("node", request.RequestID, "collection failed")
				} else {
					err = manager.Complete("node", request.RequestID, testDetailStatus())
				}

				if err != nil {
					t.Fatal(err)
				}

				if err := dispatchCtx.Err(); err != nil {
					t.Fatalf("node response canceled the command write before it returned: %v", err)
				}

				close(release)
				synctest.Wait()

				if dispatchCtx.Err() == nil {
					t.Fatal("completed dispatch retained its request context")
				}
			})
		})
	}
}
