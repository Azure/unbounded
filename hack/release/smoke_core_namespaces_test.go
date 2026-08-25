// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package release

import (
	"sort"
	"testing"
)

// Tests for smoke/core-namespaces-ready.sh.
//
// The script decides which unready pods a release may be judged on. Everything
// it excuses is a pod that would otherwise fail the soak, so every case below
// is a way of excusing one that should have failed, or of failing one that a
// site outage explains.
//
// It shares the fake kubectl in wait_rollouts_test.go rather than standing up
// its own: the stub dispatches on the shape of the call, and this script makes
// the same three kinds (get namespace, get nodes -o json, get pods).
//
// The site logic here is a deliberate second copy of the one in
// wait-rollouts.sh, so these cases deliberately overlap that file's. If one set
// changes, check the other.

// smokePod describes one pod in a core-namespaces-ready fixture.
type smokePod struct {
	name  string
	phase string
	// ready is the status of the Ready condition. Empty means the condition is
	// absent entirely, which is what a pod the kubelet never reported on looks
	// like, and what used to lose its nodeName to tab collapsing.
	ready string
	// node is .spec.nodeName. Empty means unscheduled.
	node string
	// terms become required nodeSelectorTerms, one per entry, each key/value an
	// In matchExpression. Terms are OR-ed by Kubernetes.
	terms []map[string]string
	// nodeSelector is the older spelling, AND-ed with the above.
	nodeSelector map[string]string
	// preferredSite adds a PREFERRED site affinity, which must never excuse
	// anything: the scheduler may ignore it, so the pod could have run
	// elsewhere.
	preferredSite string
}

// smokePodList renders a pod list in the shape kubectl returns.
func smokePodList(pods ...smokePod) string {
	items := make([]map[string]any, 0, len(pods))

	for _, p := range pods {
		spec := map[string]any{}
		if p.node != "" {
			spec["nodeName"] = p.node
		}

		if len(p.nodeSelector) > 0 {
			spec["nodeSelector"] = p.nodeSelector
		}

		affinity := map[string]any{}

		if len(p.terms) > 0 {
			built := make([]map[string]any, 0, len(p.terms))

			for _, term := range p.terms {
				keys := make([]string, 0, len(term))
				for key := range term {
					keys = append(keys, key)
				}

				sort.Strings(keys)

				expressions := make([]map[string]any, 0, len(term))
				for _, key := range keys {
					expressions = append(expressions, map[string]any{
						"key":      key,
						"operator": "In",
						"values":   []string{term[key]},
					})
				}

				built = append(built, map[string]any{"matchExpressions": expressions})
			}

			affinity["requiredDuringSchedulingIgnoredDuringExecution"] = map[string]any{
				"nodeSelectorTerms": built,
			}
		}

		if p.preferredSite != "" {
			affinity["preferredDuringSchedulingIgnoredDuringExecution"] = []map[string]any{{
				"weight": 100,
				"preference": map[string]any{
					"matchExpressions": []map[string]any{{
						"key":      "unbounded-cloud.io/site",
						"operator": "In",
						"values":   []string{p.preferredSite},
					}},
				},
			}}
		}

		if len(affinity) > 0 {
			spec["affinity"] = map[string]any{"nodeAffinity": affinity}
		}

		status := map[string]any{"phase": p.phase}
		if p.ready != "" {
			status["conditions"] = []map[string]any{
				{"type": "PodScheduled", "status": "True"},
				{"type": "Ready", "status": p.ready},
			}
		} else {
			status["conditions"] = []map[string]any{
				{"type": "PodScheduled", "status": "False"},
			}
		}

		items = append(items, map[string]any{
			"metadata": map[string]any{"name": p.name},
			"spec":     spec,
			"status":   status,
		})
	}

	return marshal(map[string]any{"items": items})
}

// smokeNodes is the cluster every case below runs against unless it needs
// another shape: one healthy site, one entirely unreachable.
func smokeNodes() string {
	return nodeList(
		node{name: "node-a", site: "hq", ready: "True"},
		node{name: "spark-3d37", site: "boulderlab", ready: "Unknown"},
	)
}

// runSmoke drives the real script against the fake kubectl.
func runSmoke(t *testing.T, nodes, pods string) (string, int) {
	t.Helper()

	f := newFake(t)
	f.set("get-namespace", reply{stdout: "namespace/unbounded-system"})
	f.set("getjson-nodes", reply{stdout: nodes})
	f.set("pods", reply{stdout: pods})

	return f.runScript("smoke/core-namespaces-ready.sh", map[string]string{
		"TAG":       releaseTag,
		"SITE_NAME": "stable",
	})
}

// pinnedToDeadSite is the pod the whole feature exists for: the operator's
// per-Site Deployment could not schedule, because its site is gone.
var pinnedToDeadSite = smokePod{
	name:  "metalman-controller-boulderlab-abc",
	phase: "Pending",
	terms: []map[string]string{{"unbounded-cloud.io/site": "boulderlab"}},
}

func TestSmokeToleratesAnUnscheduledPodPinnedToADeadSite(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runSmoke(t, smokeNodes(), smokePodList(pinnedToDeadSite))

	requireCode(t, code, 0, output)
	requireContains(t, output, "unscheduled; pinned to unreachable site(s) boulderlab")
	requireNotContains(t, output, "::error::")
}

func TestSmokeToleratesAPodPinnedToTwoDeadSites(t *testing.T) {
	requireBash(t)
	t.Parallel()

	nodes := nodeList(
		node{name: "node-a", site: "hq", ready: "True"},
		node{name: "spark-3d37", site: "boulderlab", ready: "Unknown"},
		node{name: "edge-1", site: "edge", ready: "Unknown"},
	)

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{
		{"unbounded-cloud.io/site": "boulderlab"},
		{"unbounded-cloud.io/site": "edge"},
	}

	output, code := runSmoke(t, nodes, smokePodList(pod))

	requireCode(t, code, 0, output)
	requireContains(t, output, "unreachable site(s) boulderlab,edge")
}

// TestSmokeRefusesAPodWhenOneOfItsSitesIsReachable is the OR condition: the
// terms are alternatives, so one reachable site means the pod could have run.
func TestSmokeRefusesAPodWhenOneOfItsSitesIsReachable(t *testing.T) {
	requireBash(t)
	t.Parallel()

	nodes := nodeList(
		node{name: "node-a", site: "hq", ready: "True"},
		node{name: "spark-3d37", site: "boulderlab", ready: "Unknown"},
		node{name: "edge-1", site: "edge", ready: "True"},
	)

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{
		{"unbounded-cloud.io/site": "boulderlab"},
		{"unbounded-cloud.io/site": "edge"},
	}

	output, code := runSmoke(t, nodes, smokePodList(pod))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod")
}

// TestSmokeRefusesAPodWhoseTermCarriesNoSite is the regression guard for the OR
// under-approximation. Smoke has no equivalent of the gate's "no pod on a
// reachable node" contradiction check, because it judges each pod alone and an
// unscheduled pod is on no node by definition. Nothing else would catch this.
func TestSmokeRefusesAPodWhoseTermCarriesNoSite(t *testing.T) {
	requireBash(t)
	t.Parallel()

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{
		{"unbounded-cloud.io/site": "boulderlab"},
		{"kubernetes.io/arch": "amd64"},
	}

	output, code := runSmoke(t, smokeNodes(), smokePodList(pod))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod")
	requireNotContains(t, output, "unscheduled; pinned to unreachable")
}

func TestSmokeToleratesAPodPinnedByTheDeprecatedSiteKey(t *testing.T) {
	requireBash(t)
	t.Parallel()

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{{"net.unbounded-cloud.io/site": "boulderlab"}}

	output, code := runSmoke(t, smokeNodes(), smokePodList(pod))

	requireCode(t, code, 0, output)
	requireContains(t, output, "unreachable site(s) boulderlab")
}

func TestSmokeToleratesAPodPinnedByNodeSelector(t *testing.T) {
	requireBash(t)
	t.Parallel()

	pod := smokePod{
		name:         "metalman-controller-boulderlab-abc",
		phase:        "Pending",
		nodeSelector: map[string]string{"unbounded-cloud.io/site": "boulderlab"},
	}

	output, code := runSmoke(t, smokeNodes(), smokePodList(pod))

	requireCode(t, code, 0, output)
	requireContains(t, output, "unreachable site(s) boulderlab")
}

// TestSmokeRefusesAnUnscheduledPodWithNoSitePin covers the ordinary
// unschedulable pod: no resources, a taint, a typo'd selector. Nothing about a
// dead site explains it.
func TestSmokeRefusesAnUnscheduledPodWithNoSitePin(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runSmoke(t, smokeNodes(), smokePodList(smokePod{
		name:  "orphan",
		phase: "Pending",
	}))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod unbounded-system/orphan not ready")
}

func TestSmokeRefusesAPodPinnedToASiteWithAReadyNode(t *testing.T) {
	requireBash(t)
	t.Parallel()

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{{"unbounded-cloud.io/site": "hq"}}

	output, code := runSmoke(t, smokeNodes(), smokePodList(pod))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod")
}

// TestSmokeRefusesAPodPinnedToASiteWithNoNodes covers the typo case, and that
// it says so: a label matching nothing is not a dead site, and an operator
// staring at a plain "not ready" would have no way to tell the difference.
func TestSmokeRefusesAPodPinnedToASiteWithNoNodes(t *testing.T) {
	requireBash(t)
	t.Parallel()

	pod := pinnedToDeadSite
	pod.terms = []map[string]string{{"unbounded-cloud.io/site": "atlantis"}}

	output, code := runSmoke(t, smokeNodes(), smokePodList(pod))

	requireCode(t, code, 1, output)
	requireContains(t, output, "pinned to site atlantis, which has no nodes")
}

// TestSmokeRefusesAPodWithOnlyPreferredAffinity keeps preferred out. The
// scheduler may ignore it, so the pod could have run on a reachable node.
func TestSmokeRefusesAPodWithOnlyPreferredAffinity(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runSmoke(t, smokeNodes(), smokePodList(smokePod{
		name:          "soft",
		phase:         "Pending",
		preferredSite: "boulderlab",
	}))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod unbounded-system/soft not ready")
}

// TestSmokeRefusesAnUnreadyPodOnAReadyNode is the behavior that existed before
// any of this and must survive it: a pod failing on a healthy node is the
// regression a release smoke test exists to catch.
func TestSmokeRefusesAnUnreadyPodOnAReadyNode(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runSmoke(t, smokeNodes(), smokePodList(smokePod{
		name: "broken", phase: "Running", ready: "False", node: "node-a",
	}))

	requireCode(t, code, 1, output)
	requireContains(t, output, "::error::pod unbounded-system/broken not ready")
}

// TestSmokeToleratesAnUnreadyPodOnANotReadyNode is the other pre-existing
// behavior. It also covers the pod shape that used to lose its nodeName to tab
// collapsing: no Ready condition at all, with a nodeName after it.
func TestSmokeToleratesAnUnreadyPodOnANotReadyNode(t *testing.T) {
	requireBash(t)
	t.Parallel()

	output, code := runSmoke(t, smokeNodes(), smokePodList(smokePod{
		name: "stranded", phase: "Running", node: "spark-3d37",
	}))

	requireCode(t, code, 0, output)
	requireContains(t, output, "stranded: unbounded-system/stranded on NotReady node spark-3d37")
}

// TestSmokeFailsClosedWhenNodeReadinessCannotBeRead covers the contract stated
// at the top of the script: if node readiness cannot be established, every
// unready pod is judged. Excusing pods against an unknown cluster would be the
// worst of both.
func TestSmokeFailsClosedWhenNodeReadinessCannotBeRead(t *testing.T) {
	requireBash(t)
	t.Parallel()

	f := newFake(t)
	f.set("get-namespace", reply{stdout: "namespace/unbounded-system"})
	f.set("getjson-nodes", reply{stderr: "error: the server could not find the requested resource", exit: 1})
	f.set("pods", reply{stdout: smokePodList(pinnedToDeadSite)})

	output, code := f.runScript("smoke/core-namespaces-ready.sh", map[string]string{
		"TAG":       releaseTag,
		"SITE_NAME": "stable",
	})

	requireCode(t, code, 1, output)
	requireContains(t, output, "could not list node readiness")
}
