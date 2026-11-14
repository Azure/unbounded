// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"path/filepath"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/hostnode"
	"github.com/Azure/unbounded/pkg/agent/noneops"
)

func TestResetRemovesIPAMReservations(t *testing.T) {
	t.Parallel()

	f := newNodeFixture(t, "1.37.0")
	if err := f.node.Provision(t.Context()); err != nil {
		t.Fatal(err)
	}

	lease := filepath.Join(f.root, hostnode.CNIStateDir, "bridge", "10.185.0.2")
	mustWrite(t, lease, "ended-pod")

	td := &noneops.Teardown{
		StateDir: f.node.p.StateDir,
		Log:      f.node.p.Log, Root: f.root, Layout: f.node.p.Layout,
		SystemdDir: f.units, Run: f.host.run, Proc: filepath.Join(f.root, "proc"),
		MountInfo: filepath.Join(f.root, "mountinfo"),
	}
	for range 2 {
		if err := td.Destroy().Do(t.Context()); err != nil {
			t.Fatal(err)
		}

		assertExists(t, lease, false)
	}
}
