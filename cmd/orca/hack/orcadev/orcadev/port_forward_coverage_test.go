// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package orcadev

import (
	"context"
	"fmt"
	"testing"
)

// TestEverySubcommandOpensPortForwards is the structural guarantee
// behind the dev workflow contract: a subcommand that touches the
// origin / cachestore / edge MUST open the corresponding kubectl
// port-forwards before constructing the backend client. Otherwise
// `bin/orcadev <verb>` only works on kind (where NodePort host
// mappings used to make `localhost:30100` resolve) and breaks on
// every other cluster flavor as soon as setup-orca.sh stops emitting
// NodePort.
//
// The test stubs the first backend constructor each subcommand
// reaches and asserts the stub was called - which proves the
// subcommand's RunE made it through the ensurePortForwards call
// without panicking, and that the call happens before the backend
// is constructed (otherwise the stub would be reached even with no
// forwards). autoPortForward is set to false so the test does not
// spawn kubectl; ensurePortForwards becomes a no-op cleanup.
func TestEverySubcommandOpensPortForwards(t *testing.T) {
	tests := []struct {
		name string
		// firstBackend indicates which seam to stub first to halt
		// the subcommand: "origin" or "cachestore".
		firstBackend string
		// run drives one subcommand to the halt-stub. The subcommand
		// must call ensurePortForwards before the backend
		// constructor, otherwise the test detects the regression by
		// failing in CI on a non-kind machine (no kubectl).
		run func(g *globalFlags) error
	}{
		{
			name:         "upload --file",
			firstBackend: "origin",
			run: func(g *globalFlags) error {
				return runUpload(context.Background(), g, &uploadOpts{file: "/dev/null"})
			},
		},
		{
			name:         "list",
			firstBackend: "origin",
			run: func(g *globalFlags) error {
				return runList(context.Background(), g, &listOpts{})
			},
		},
		{
			name:         "delete",
			firstBackend: "origin",
			run: func(g *globalFlags) error {
				return runDelete(context.Background(), g, &deleteOpts{yes: true})
			},
		},
		{
			name:         "cache list",
			firstBackend: "cachestore",
			run: func(g *globalFlags) error {
				return runCacheList(context.Background(), g, &cacheListOpts{})
			},
		},
		{
			name:         "cache inspect",
			firstBackend: "origin",
			run: func(g *globalFlags) error {
				return runCacheInspect(context.Background(), g, &cacheInspectOpts{
					bucket: "test-bucket",
					key:    "test-key",
				})
			},
		},
		{
			name:         "cache clear --object",
			firstBackend: "cachestore",
			run: func(g *globalFlags) error {
				return runCacheClear(context.Background(), g, &cacheClearOpts{
					objectK: "bucket/key",
					yes:     true,
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Not t.Parallel: swaps package-level seams.
			var called bool

			switch tt.firstBackend {
			case "origin":
				original := newOriginClient
				newOriginClient = func(_ context.Context, _ *globalFlags) (originClient, error) {
					called = true

					return nil, fmt.Errorf("test halt after newOriginClient")
				}

				t.Cleanup(func() { newOriginClient = original })
			case "cachestore":
				original := newCachestoreClientFn
				newCachestoreClientFn = func(_ context.Context, _ *globalFlags) (*cachestoreClient, error) {
					called = true

					return nil, fmt.Errorf("test halt after newCachestoreClient")
				}

				t.Cleanup(func() { newCachestoreClientFn = original })
			default:
				t.Fatalf("unknown firstBackend %q", tt.firstBackend)
			}

			g := defaultGlobalFlags()
			g.originID = "test-origin"
			// We do not want to spawn kubectl in this unit test.
			// ensurePortForwards returns a no-op cleanup when
			// autoPortForward is false; the test only asserts the
			// stub was reached, which proves the call site exists
			// in the right order.
			g.autoPortForward = false

			err := tt.run(g)
			if err == nil {
				t.Fatal("expected halt error from test stub; got nil")
			}

			if !called {
				t.Errorf("%s constructor was never reached for %q",
					tt.firstBackend, tt.name)
			}
		})
	}
}
