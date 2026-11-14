// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func TestRepaveReapsOnlyAgentShims(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, comm, cgroup string
		want               bool
	}{
		{name: "owned", comm: "containerd-shim", cgroup: "0::/system.slice/" + none.UnitContainerd, want: true},
		{name: "owned descendant", comm: "containerd-shim", cgroup: "0::/system.slice/" + none.UnitContainerd + "/child", want: true},
		{name: "other runtime", comm: "containerd-shim", cgroup: "0::/system.slice/containerd.service"},
		{name: "similar unit", comm: "containerd-shim", cgroup: "0::/system.slice/other-" + none.UnitContainerd},
		{name: "not shim", comm: "unrelated", cgroup: "0::/system.slice/" + none.UnitContainerd},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proc := t.TempDir()
			mustWrite(t, filepath.Join(proc, "123", "comm"), tc.comm)
			mustWrite(t, filepath.Join(proc, "123", "cgroup"), tc.cgroup)

			var killed []int

			backend := &none.Backend{Proc: proc, Kill: func(pid int) error { killed = append(killed, pid); return nil }}
			if err := backend.ReapPods(); err != nil {
				t.Fatal(err)
			}

			if slices.Contains(killed, 123) != tc.want {
				t.Fatalf("killed = %v, owned = %t", killed, tc.want)
			}
		})
	}
}

func TestRepaveReapsOnlyPodCgroups(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "kubepods.slice", "pod", "cgroup.procs"), "123\n")
	mustWrite(t, filepath.Join(root, "system.slice", "ssh.service", "cgroup.procs"), "456\n")

	var killed []int

	backend := &none.Backend{CgroupRoot: root, Kill: func(pid int) error { killed = append(killed, pid); return nil }}
	if err := backend.ReapPods(); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(killed, []int{123}) {
		t.Fatalf("killed = %v, want only pod process 123", killed)
	}
}
