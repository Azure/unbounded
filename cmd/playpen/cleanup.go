// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
)

// runCleanup removes the shared resources playpen deliberately never cleans up
// during normal operation: the reaper RBAC (ClusterRoleBinding, ClusterRole, and
// ServiceAccount), the bootstrapped Site and SiteGatewayPoolAssignment, and the
// shared namespace. These are created if missing by `up` and reused across every
// run in a namespace scope, so they outlive individual runs on purpose.
//
// To avoid orphaning per-run resources under a namespace scope that is about to
// lose its shared RBAC, cleanup first deletes every playpen run in the scope
// (their Node anchors cascade to pods and per-run RBAC), then removes the shared
// objects.
func runCleanup(ctx context.Context, cfg Config) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}

	deletedRuns, err := deleteAllRuns(ctx, c, cfg.Namespace)
	if err != nil {
		return err
	}

	if len(deletedRuns) > 0 {
		fmt.Printf("deleted %d playpen run(s): %v\n", len(deletedRuns), deletedRuns)
	}

	deletedShared, err := deleteSharedResources(ctx, c, cfg)
	if err != nil {
		return err
	}

	if len(deletedShared) == 0 {
		fmt.Printf("no shared playpen resources found in namespace scope %q\n", cfg.Namespace)
		return nil
	}

	fmt.Printf("deleted %d shared playpen resource(s):\n", len(deletedShared))

	for _, d := range deletedShared {
		fmt.Printf("  - %s\n", d)
	}

	return nil
}
