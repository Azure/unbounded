// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/cmd/agent/internal/hostnode"
	"github.com/Azure/unbounded/cmd/agent/internal/hostnode/hostnodetest"
	"github.com/Azure/unbounded/cmd/agent/internal/noneops"
	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/config"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/isolation"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

// ---- none plan ----

type fakeHost struct {
	mu     sync.Mutex
	calls  []string
	active map[string]bool
}

func (h *fakeHost) run(_ context.Context, _ *slog.Logger, name string, args ...string) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.calls = append(h.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))

	if name == "systemctl" && len(args) > 1 {
		unit := args[len(args)-1]

		switch args[0] {
		case "enable", "restart":
			h.active[unit] = true
		case "is-active":
			if !h.active[unit] {
				return "", errors.New("inactive")
			}
		}
	}

	return "", nil
}

func newNonePlan(t *testing.T, running bool) (*nonePlan, *agentStages, *fakeHost, *[]string) {
	t.Helper()

	h := &fakeHost{active: map[string]bool{}}
	if running {
		h.active[none.UnitContainerd] = true
		h.active[none.UnitKubelet] = true
	}

	root := t.TempDir()
	gs := hostnodetest.MinimalGoalState()

	var persisted []string

	p := &nonePlan{hostSeams: hostSeams{
		sync: func(...string) error { return nil },
		archives: func(*slog.Logger, *goalstates.ContainerImageArchiveStaging) phases.Task {
			return hostnode.Func("archives", func(context.Context) error { return nil })
		},
		persist: func(_ *slog.Logger, mode isolation.Mode, slot string, _ *config.AgentConfig) phases.Task {
			return hostnode.Func("persist-applied-config", func(context.Context) error {
				persisted = append(persisted, goalstates.AppliedConfigName(mode, slot))
				return nil
			})
		},
	}, ownership: func() error { return nil }}
	p.node = noneops.New(noneops.Params{
		Log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		GoalState: gs,
		Overrides: noneops.Overrides{Layout: none.Layout{Base: filepath.Join(root, "node")}, SystemdDir: filepath.Join(root, "units"), Run: h.run, Proc: filepath.Join(root, "noproc"), ImageArchiveDir: filepath.Join(root, "images"), Download: func(context.Context) error { return nil }, WaitKubelet: func(context.Context) error { return nil }},
		Hooks:     hostnode.Hooks{HostRoot: filepath.Join(root, "host"), SetupNetwork: func(context.Context) error { return nil }},
	})

	s := &agentStages{
		log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		cfg:              &provision.UnboundedAgentConfig{},
		gs:               gs,
		credentialsReady: true,
		stagePlan:        p,
	}

	return p, s, h, &persisted
}

func TestNonePlanPrepareNodeRootInstallsAndActivates(t *testing.T) {
	p, s, _, _ := newNonePlan(t, false)

	require.NoError(t, s.PrepareRootFS(t.Context()))

	v, err := p.node.Version()
	require.NoError(t, err)

	b, err := p.node.Backend()
	require.NoError(t, err)

	cur, err := b.Layout.Current()
	require.NoError(t, err)
	require.Equal(t, v, cur, "bootstrap activates the version it installed")
	require.FileExists(t, filepath.Join(b.SystemdDir, none.UnitKubelet))
}

func TestNonePlanPrepareNodeRootLeavesARunningNodeAlone(t *testing.T) {
	p, s, h, _ := newNonePlan(t, true)

	require.NoError(t, s.PrepareRootFS(t.Context()))

	b, err := p.node.Backend()
	require.NoError(t, err)
	require.NoDirExists(t, b.Layout.VersionDir("anything"))
	require.NoFileExists(t, filepath.Join(b.SystemdDir, none.UnitKubelet), "nothing was rewritten")

	for _, c := range h.calls {
		require.NotContains(t, c, "daemon-reload")
	}
}

func TestNonePlanEnsureNodeStartedPersistsAppliedConfigOnlyForANodeItBuilt(t *testing.T) {
	_, s, h, persisted := newNonePlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	require.NoError(t, s.EnsureNodeStarted(t.Context()))
	require.Equal(t, []string{"none-kube1"}, *persisted, "recorded where agents that predate none never look")
	require.True(t, h.active[none.UnitKubelet])

	// A retry that finds the node standing did not build it.
	_, s2, _, persisted2 := newNonePlan(t, true)
	require.NoError(t, s2.EnsureNodeStarted(t.Context()))
	require.Empty(t, *persisted2)
}

func TestNonePlanEnsureNodeStartedFailureNamesTheTaskAndRecordsNothing(t *testing.T) {
	_, s, h, persisted := newNonePlan(t, false)
	require.NoError(t, s.PrepareRootFS(t.Context()))

	// Fail the kubelet start; the units before it started.
	p := s.stagePlan.(*nonePlan)
	b, err := p.node.Backend()
	require.NoError(t, err)

	b.Run = func(ctx context.Context, l *slog.Logger, name string, args ...string) (string, error) {
		if name == "systemctl" && len(args) == 3 && args[2] == none.UnitKubelet && args[0] == "enable" {
			return "", errors.New("exec format error")
		}

		return h.run(ctx, l, name, args...)
	}

	err = s.EnsureNodeStarted(t.Context())
	require.ErrorContains(t, err, "start-none-node")
	require.ErrorContains(t, err, "exec format error")
	require.Empty(t, *persisted, "a node that never started is not recorded as built")
	require.Equal(t, "Failed", classifyNodeStartFailure(err))

	// A node that starts but whose kubelet never bootstraps is the common real
	// failure and keeps its dedicated reason.
	require.Equal(t, "KubeletBootstrapFailed", classifyNodeStartFailure(
		errors.New("start-none-node: wait-for-kubelet-bootstrap: kubelet did not bootstrap")))
}

// The ownership check is part of EnsureHostClean, not only of the optional
// preflight subcommand: the plan neither stops nor disables a distro kubelet or
// containerd, so a host that has one must be refused before anything changes.
func TestNonePlanEnsureHostCleanRunsTheOwnershipCheck(t *testing.T) {
	p, s, _, _ := newNonePlan(t, false)
	s.cfg = &provision.UnboundedAgentConfig{AgentConfig: config.AgentConfig{Isolation: "none"}}

	called := false
	p.ownership = func() error {
		called = true

		return errors.New("kubelet unit /lib/systemd/system/kubelet.service exists and is not managed by the agent")
	}

	err := s.EnsureHostClean(t.Context())
	if err != nil && strings.Contains(err.Error(), "existing node deployment detected") {
		t.Skip("the test host has node artifacts; the ownership check is not reached")
	}

	require.True(t, called)
	require.ErrorContains(t, err, "needs a host without a kubelet or containerd of its own")
	require.ErrorContains(t, err, "kubelet.service")

	p.ownership = func() error { return nil }

	require.NoError(t, s.EnsureHostClean(t.Context()))
}

// prepareHostCalls returns the host.* constructors called inside the
// PrepareHost method of the plan type in file. PrepareHost runs real host
// steps and exposes no seam to inspect the steps it assembles, so the
// composition is read from the source.
func prepareHostCalls(t *testing.T, file, planType string) []string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	require.NoError(t, err)

	var (
		calls []string
		found bool
	)

	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "PrepareHost" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}

		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}

		if ident, ok := recv.(*ast.Ident); !ok || ident.Name != planType {
			continue
		}

		found = true

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "host" {
						calls = append(calls, sel.Sel.Name)
					}
				}
			}

			return true
		})
	}

	require.True(t, found, "%s has no PrepareHost method on %s", file, planType)

	return calls
}

// The nspawn plan hardens APT against machined restarts, a step that exists
// only for the machine. The none plan has no machine: it does not harden, and
// it removes the guards first boot wrote for every mode.
func TestOnlyTheNSpawnPlanHardensAPT(t *testing.T) {
	t.Parallel()

	nspawnSrc, err := os.ReadFile("stageplan_nspawn.go")
	require.NoError(t, err)
	require.Contains(t, string(nspawnSrc), "host.HardenAPT(s.log)")

	calls := prepareHostCalls(t, "stageplan_none.go", "nonePlan")
	require.NotContains(t, calls, "HardenAPT", "the none plan has no machined to harden against")
	require.Contains(t, calls, "RemoveAPTHardening")
	require.NotContains(t, calls, "DisableContainerd", "EnsureHostClean refused a host with one")
	require.NotContains(t, calls, "DisableKubelet", "EnsureHostClean refused a host with one")
}
