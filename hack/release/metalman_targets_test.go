// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"strings"
	"testing"
)

// Tests for metalman-targets.sh.
//
// The script decides which metalman Deployments the release gate waits on. Its
// answer is derived from cluster state, so the interesting cases are the ones
// where that state is missing, partial or unreadable: an empty answer removes
// metalman from the gate entirely, and it must only ever be reached
// deliberately.

// siteList renders a Site list in the shape kubectl returns. A nil components
// map produces a Site with no `.spec.components` at all.
func siteList(sites ...site) string {
	items := make([]map[string]any, 0, len(sites))

	for _, s := range sites {
		item := map[string]any{"metadata": map[string]any{"name": s.name}}

		switch {
		case s.noSpec:
		case s.noComponents:
			item["spec"] = map[string]any{}
		case s.emptyMetalman:
			item["spec"] = map[string]any{"components": map[string]any{"metalman": map[string]any{}}}
		default:
			item["spec"] = map[string]any{
				"components": map[string]any{"metalman": map[string]any{"enabled": s.metalman}},
			}
		}

		items = append(items, item)
	}

	return marshal(map[string]any{"items": items})
}

// site describes one fake Site. The shapes below are all reachable: a Site
// created without --enable-metalman writes `enabled: false`, one migrated by
// the reaper may predate the components block entirely.
type site struct {
	name          string
	metalman      bool
	emptyMetalman bool
	noComponents  bool
	noSpec        bool
}

// runTargets executes the script and returns stdout+stderr and the exit code.
func runTargets(t *testing.T, reply string, env map[string]string) (string, int) {
	t.Helper()

	f := newFake(t)
	if reply != "" {
		f.set("getjson-sites.unbounded-cloud.io", replyOf(reply))
	}

	return f.runScript("metalman-targets.sh", env)
}

func replyOf(stdout string) reply { return reply{stdout: stdout} }

func TestMetalmanTargetsListsEveryEnabledSite(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, siteList(
		site{name: "stable"},
		site{name: "boulderlab", metalman: true},
		site{name: "edge", metalman: true},
	), nil)

	requireCode(t, code, 0, output)
	requireContains(t, output, "deploy/metalman-controller-boulderlab")
	requireContains(t, output, "deploy/metalman-controller-edge")
	// The cluster's own Site does not enable it; unbounded-stable runs metalman
	// for a remote site, which is why the targets are not built from SITE_NAME.
	requireNotContains(t, output, "metalman-controller-stable")
}

// TestMetalmanTargetsIgnoresPartialSites covers the Site shapes that exist
// alongside a properly configured one. None of them may error, and none may be
// mistaken for an enabled Site.
func TestMetalmanTargetsIgnoresPartialSites(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, siteList(
		site{name: "disabled"},
		site{name: "empty-block", emptyMetalman: true},
		site{name: "no-components", noComponents: true},
		site{name: "no-spec", noSpec: true},
		site{name: "boulderlab", metalman: true},
	), nil)

	requireCode(t, code, 0, output)

	targets := 0

	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(line, "deploy/") {
			targets++
		}
	}

	if targets != 1 {
		t.Errorf("expected exactly one target, got %d\n--- output ---\n%s", targets, output)
	}

	requireContains(t, output, "deploy/metalman-controller-boulderlab")
}

func TestMetalmanTargetsFailsWhenNoSiteEnablesIt(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, siteList(
		site{name: "stable"},
		site{name: "edge", emptyMetalman: true},
	), nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "expected to run it")
	requireNotContains(t, output, "deploy/")
}

// TestMetalmanTargetsAllowsNoneOnFirstBootstrap covers the one state where an
// empty answer is legitimate: nothing has joined the cluster yet.
func TestMetalmanTargetsAllowsNoneOnFirstBootstrap(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, siteList(site{name: "stable"}),
		map[string]string{"REQUIRE_METALMAN": "false"})

	requireCode(t, code, 0, output)
	requireContains(t, output, "not gating on it")
	requireNotContains(t, output, "deploy/")
}

func TestMetalmanTargetsHandlesAnEmptySiteList(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, `{"items":[]}`, nil)

	requireCode(t, code, 1, output)
	requireContains(t, output, "expected to run it")
}

// TestMetalmanTargetsFailsWhenTheQueryFails is the fail-closed case: an
// unreachable apiserver must not read as "no Site enables metalman", which
// would silently drop metalman from the release gate.
func TestMetalmanTargetsFailsWhenTheQueryFails(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("getjson-sites.unbounded-cloud.io", reply{
		stderr: "error: You must be logged in to the server (Unauthorized)",
		exit:   1,
	})

	output, code := f.runScript("metalman-targets.sh", nil)

	requireCode(t, code, 3, output)
	requireContains(t, output, "could not list")
	requireContains(t, output, "Unauthorized")
	requireNotContains(t, output, "deploy/")
}

// TestMetalmanTargetsFailsOnAMalformedPayload is the same discipline one layer
// up: a payload jq cannot walk is exit 3, never a silent empty answer.
func TestMetalmanTargetsFailsOnAMalformedPayload(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, `{"unexpected":"shape"}`, nil)

	requireCode(t, code, 3, output)
	requireContains(t, output, "could not evaluate")
	requireNotContains(t, output, "deploy/")
}

func TestMetalmanTargetsRequiresKubeconfig(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runTargets(t, siteList(site{name: "boulderlab", metalman: true}),
		map[string]string{"KUBECONFIG": ""})

	requireCode(t, code, 2, output)
	requireContains(t, output, "KUBECONFIG")
}
