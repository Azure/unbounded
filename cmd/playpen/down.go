// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"fmt"
	"time"
)

// runDown deletes the cluster resources created by playpen. There is no local
// teardown: the userspace dataplane lives only inside the `up` process and is
// gone once that process exits.
//
// Deleting a run's Node anchor cascades to its Pod, ServiceAccount, ClusterRole,
// and ClusterRoleBinding via owner references. The shared namespace is never
// deleted. By default runDown targets the most recent run (recorded by `up`);
// with all set it deletes every playpen run in this namespace scope. Either
// way it also reaps any expired runs.
func runDown(ctx context.Context, cfg Config, all bool) error {
	c, err := newClient(cfg)
	if err != nil {
		return err
	}

	if all {
		deleted, err := deleteAllRuns(ctx, c, cfg.Namespace)
		if err != nil {
			return err
		}

		if len(deleted) == 0 {
			fmt.Printf("no playpen runs found in namespace scope %q\n", cfg.Namespace)
		} else {
			fmt.Printf("deleted %d playpen run(s): %v\n", len(deleted), deleted)
		}

		return nil
	}

	// Best-effort: also reap anything already past its TTL while we are here.
	if reaped, err := reapExpired(ctx, c, cfg.Namespace, time.Now()); err != nil {
		fmt.Printf("warning: reaping stale runs: %v\n", err)
	} else if len(reaped) > 0 {
		fmt.Printf("reaped %d expired playpen run(s): %v\n", len(reaped), reaped)
	}

	nodeName, err := readLastRun(cfg)
	if err != nil {
		return err
	}

	if nodeName == "" {
		fmt.Printf("no recorded run to delete; use --all to delete every playpen run in namespace scope %q\n", cfg.Namespace)
		return nil
	}

	if err := deleteNode(ctx, c, nodeName); err != nil {
		return err
	}

	fmt.Printf("deleted run %q (pod, service account, and RBAC cascade from the node anchor)\n", nodeName)

	return nil
}
