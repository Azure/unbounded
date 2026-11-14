// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package transaction

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

func runBootGuard(f nodeFixture) error {
	state := filepath.Dir(f.node.configSnapshotDir())
	helper := filepath.Join(filepath.Dir(f.node.p.Layout.CurrentLink()), ".check-startup")

	return exec.Command(helper, filepath.Join(state, "none-reset"), f.node.journalPath(), f.node.startupPath()).Run()
}

func TestBootGuard(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name                      string
		preparing, exposed, reset bool
		blocked                   bool
	}{
		{name: "normal"},
		{name: "preparing", preparing: true, blocked: true},
		{name: "prepared startup", preparing: true, exposed: true},
		{name: "reset", reset: true, blocked: true},
		{name: "reset after exposure", preparing: true, exposed: true, reset: true, blocked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newNodeFixture(t, "1.37.0")
			if err := f.node.EnsureBootGuard(t.Context()); err != nil {
				t.Fatal(err)
			}

			state := filepath.Dir(f.node.configSnapshotDir())
			if tc.preparing {
				mustWrite(t, f.node.journalPath(), "preparing")
			}

			if tc.exposed {
				mustWrite(t, f.node.startupPath(), "prepared")
			}

			if tc.reset {
				mustWrite(t, filepath.Join(state, "none-reset"), "resetting")
			}

			if err := runBootGuard(f); (err != nil) != tc.blocked {
				t.Fatalf("boot guard = %v, blocked=%t", err, tc.blocked)
			}

			guard := filepath.Join(f.units, none.UnitKubelet+".d", "01-startup-guard.conf")

			data, err := os.ReadFile(guard)
			if err != nil || !strings.Contains(string(data), "ExecCondition=") {
				t.Fatalf("missing systemd gate: %v", err)
			}
		})
	}
}

func TestPreparingWindowHasBootGuardBeforeActivation(t *testing.T) {
	t.Parallel()
	f := newNodeFixture(t, "1.37.0")
	old := installOld(t, f)
	p := f.node.p
	observed := false
	p.SetupNetwork = func(context.Context) error {
		current, err := p.Layout.Current()
		if err != nil {
			t.Fatal(err)
		}

		if current != old {
			observed = true

			if err := runBootGuard(f); err == nil {
				t.Fatal("preparing target can start at boot without startup intent")
			}
		}

		return errors.New("test interruption before kubelet exposure")
	}

	f.node = New(p)
	if err := f.node.Upgrade(t.Context()); err == nil {
		t.Fatal("expected injected failure")
	}

	if !observed {
		t.Fatal("test did not reach target activation")
	}
}

func TestSystemdBootGuardArgumentEscaping(t *testing.T) {
	t.Parallel()

	value := `/tmp/space dir/"$HOME"/%i`

	got := systemdArgument(value)
	if !strings.Contains(got, `$$HOME`) || !strings.Contains(got, `%%i`) || !strings.Contains(got, `\"`) {
		t.Fatalf("unsafe systemd argument: %s", got)
	}
}
