// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/internal/operator/component"
)

// siteAffinity mirrors component.SiteNodeAffinity: two OR'd terms, matching the
// canonical and the deprecated Site label. Per-Site workloads always carry it.
func siteAffinity(site string) map[string]any {
	term := func(key string) any {
		return map[string]any{
			"matchExpressions": []any{
				map[string]any{
					"key":      key,
					"operator": "In",
					"values":   []any{site},
				},
			},
		}
	}

	return map[string]any{
		"nodeAffinity": map[string]any{
			"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
				"nodeSelectorTerms": []any{
					term("unbounded-cloud.io/site"),
					term("net.unbounded-cloud.io/site"),
				},
			},
		},
	}
}

// testWorkload builds a DaemonSet shaped like the ones the operator generates:
// a selector, matching template labels, a container, an operator volume mount,
// and per-Site node affinity.
func testWorkload(site string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "DaemonSet",
		"metadata": map[string]any{
			"name":      "unbounded-storage-supervisor-" + site,
			"namespace": "unbounded-system",
			"labels":    map[string]any{"app.kubernetes.io/name": "storage"},
			"ownerReferences": []any{
				map[string]any{
					"apiVersion": "unbounded-cloud.io/v1alpha3",
					"kind":       "Site",
					"name":       site,
					"uid":        "uid-" + site,
					"controller": true,
				},
			},
		},
		"spec": map[string]any{
			"selector": map[string]any{
				"matchLabels": map[string]any{
					"app.kubernetes.io/name":  "storage",
					"unbounded-cloud.io/site": site,
				},
			},
			"template": map[string]any{
				"metadata": map[string]any{
					"labels": map[string]any{
						"app.kubernetes.io/name":  "storage",
						"unbounded-cloud.io/site": site,
					},
					"annotations": map[string]any{
						"unbounded-cloud.io/storage-config-hash": "abc123",
					},
				},
				"spec": map[string]any{
					"affinity": siteAffinity(site),
					"containers": []any{
						map[string]any{
							"name":  "run",
							"image": "ghcr.io/azure/unbounded-storage-supervisor:v1",
							"args":  []any{"--config=/etc/storage/config.yaml"},
							"volumeMounts": []any{
								map[string]any{"name": "storage-config", "mountPath": "/etc/storage"},
							},
						},
					},
					"volumes": []any{
						map[string]any{
							"name":      "storage-config",
							"configMap": map[string]any{"name": "unbounded-storage-config-" + site},
						},
					},
				},
			},
		},
	}}

	return obj
}

// planWith wraps a workload in a single-operation plan marked overridable.
func planWith(workload *unstructured.Unstructured, componentName, site string) *component.Plan {
	plan := component.NewPlan()
	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      workload,
		Component:   componentName,
		Site:        site,
		Overridable: true,
	})

	return plan
}

// entriesFrom parses a whole document the way the operator does.
func entriesFrom(t *testing.T, doc string) []SourcedEntry {
	t.Helper()

	entries, err := parseAll(map[string]string{"overrides.yaml": doc})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := ValidateErr(entries); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	return entries
}

func doc(body string) string {
	return "apiVersion: " + APIVersion + "\noverrides:\n" + body
}

// podSpec decodes the merged pod spec so assertions read naturally.
func podSpec(t *testing.T, workload *unstructured.Unstructured) corev1.PodSpec {
	t.Helper()

	raw, found, err := unstructured.NestedMap(workload.Object, "spec", "template", "spec")
	if err != nil || !found {
		t.Fatalf("pod spec not found: %v", err)
	}

	encoded, err := yaml.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal pod spec: %v", err)
	}

	var spec corev1.PodSpec
	if err := yaml.Unmarshal(encoded, &spec); err != nil {
		t.Fatalf("decode pod spec: %v", err)
	}

	return spec
}

func TestApplyMergesResources(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                resources:
                  limits:
                    memory: 512Mi
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if got := spec.Containers[0].Resources.Limits.Memory().String(); got != "512Mi" {
		t.Fatalf("memory limit = %q, want 512Mi", got)
	}

	// The operator's own args must survive a resources-only patch.
	if len(spec.Containers[0].Args) != 1 {
		t.Fatalf("args = %v, want the operator's args preserved", spec.Containers[0].Args)
	}
}

// TestApplyPreservesSiteAffinity is the Site-isolation regression test.
//
// SiteNodeAffinity emits two OR'd terms and NodeSelectorTerms carries no
// patchMergeKey, so a raw strategic merge would replace them outright and let
// two Sites' workloads schedule onto the same nodes.
func TestApplyPreservesSiteAffinity(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: disktype
                          operator: In
                          values: [ssd]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("terms = %d, want 2 (two operator terms x one user term)", len(terms))
	}

	// Every product term must carry both the Site constraint and the user's.
	for i, term := range terms {
		var sawSite, sawUser bool

		for _, expr := range term.MatchExpressions {
			switch expr.Key {
			case "unbounded-cloud.io/site", "net.unbounded-cloud.io/site":
				sawSite = true

				if expr.Values[0] != "rack-a" {
					t.Fatalf("term %d Site value = %v, want rack-a", i, expr.Values)
				}
			case "disktype":
				sawUser = true
			}
		}

		if !sawSite {
			t.Fatalf("term %d lost the Site constraint: %+v", i, term.MatchExpressions)
		}

		if !sawUser {
			t.Fatalf("term %d lost the user constraint: %+v", i, term.MatchExpressions)
		}
	}
}

// TestApplyAffinityIsCartesian covers the case appending would get wrong: two
// operator terms combined with two user terms must produce four, not two.
func TestApplyAffinityIsCartesian(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            affinity:
              nodeAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  nodeSelectorTerms:
                    - matchExpressions:
                        - key: disktype
                          operator: In
                          values: [ssd]
                    - matchFields:
                        - key: metadata.name
                          operator: In
                          values: [node-1]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)

	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 4 {
		t.Fatalf("terms = %d, want 4 (2 operator x 2 user)", len(terms))
	}

	// matchFields must survive: dropping it would silently discard a user
	// constraint, since NodeSelectorTerm carries both lists.
	var sawMatchFields bool

	for _, term := range terms {
		if len(term.MatchFields) > 0 {
			sawMatchFields = true
		}
	}

	if !sawMatchFields {
		t.Fatal("matchFields was dropped from the product")
	}
}

// TestApplyAppendsTolerations covers the other departure from raw strategic
// merge: tolerations carry no merge key, so a merge would replace the
// operator's list.
func TestApplyAppendsTolerations(t *testing.T) {
	workload := testWorkload("rack-a")

	_ = unstructured.SetNestedSlice(workload.Object, []any{
		map[string]any{"key": "operator-owned", "operator": "Exists"},
	}, "spec", "template", "spec", "tolerations")

	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            tolerations:
              - key: edge
                operator: Exists
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if len(spec.Tolerations) != 2 {
		t.Fatalf("tolerations = %+v, want the operator's and the user's", spec.Tolerations)
	}
}

// TestApplyRejectsOverwritingOperatorNodeSelector covers the one nodeSelector
// case that is not additive.
func TestApplyRejectsOverwritingOperatorNodeSelector(t *testing.T) {
	workload := testWorkload("rack-a")

	_ = unstructured.SetNestedMap(workload.Object, map[string]any{"kubernetes.io/os": "linux"},
		"spec", "template", "spec", "nodeSelector")

	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              kubernetes.io/os: windows
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("overwriting an operator-set nodeSelector key must fail")
	}

	if !strings.Contains(report.Err().Error(), "set by the operator") {
		t.Fatalf("error = %v, want it to explain the operator owns the key", report.Err())
	}
}

// TestApplyRestampsIdentity is one of three independent layers protecting the
// group, version and kind. Validation rejects such a patch, but correctness
// must not depend on validation being exhaustive.
func TestApplyRestampsIdentity(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	// Bypass Validate deliberately: this asserts the re-stamp, not the check.
	entries := []SourcedEntry{{
		Source: Source{Key: "evil.yaml", Index: 0},
		Entry: Entry{
			Component: "storage",
			Kind:      "DaemonSet",
			Patch: map[string]any{
				"apiVersion": "rbac.authorization.k8s.io/v1",
				"kind":       "ClusterRoleBinding",
				"metadata": map[string]any{
					"name":            "pwned",
					"namespace":       "kube-system",
					"ownerReferences": []any{},
				},
				"spec": map[string]any{
					"selector": map[string]any{"matchLabels": map[string]any{"a": "b"}},
				},
			},
		},
	}}

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	got := plan.Operations[0].Object

	if got.GetAPIVersion() != "apps/v1" || got.GetKind() != "DaemonSet" {
		t.Fatalf("GVK = %s %s, want apps/v1 DaemonSet", got.GetAPIVersion(), got.GetKind())
	}

	if got.GetName() != "unbounded-storage-supervisor-rack-a" || got.GetNamespace() != "unbounded-system" {
		t.Fatalf("identity = %s/%s, want the operator's", got.GetNamespace(), got.GetName())
	}

	if len(got.GetOwnerReferences()) != 1 {
		t.Fatalf("ownerReferences = %+v, want the Site owner restored", got.GetOwnerReferences())
	}

	selector, _, _ := unstructured.NestedStringMap(got.Object, "spec", "selector", "matchLabels")
	if selector["unbounded-cloud.io/site"] != "rack-a" {
		t.Fatalf("selector = %v, want the operator's", selector)
	}
}

// TestApplyKeepsSelectorMatchingTemplateLabels guards against producing a
// workload the API server rejects outright.
func TestApplyKeepsSelectorMatchingTemplateLabels(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          metadata:
            labels:
              team: platform
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	labels, _, _ := unstructured.NestedStringMap(plan.Operations[0].Object.Object,
		"spec", "template", "metadata", "labels")

	for key, want := range map[string]string{
		"app.kubernetes.io/name":  "storage",
		"unbounded-cloud.io/site": "rack-a",
		"team":                    "platform",
	} {
		if labels[key] != want {
			t.Fatalf("template label %q = %q, want %q (labels=%v)", key, labels[key], want, labels)
		}
	}
}

func TestApplyExtraArgsAppends(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--verbose"]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)

	want := []string{"--config=/etc/storage/config.yaml", "--verbose"}
	if len(spec.Containers[0].Args) != len(want) {
		t.Fatalf("args = %v, want %v", spec.Containers[0].Args, want)
	}

	for i := range want {
		if spec.Containers[0].Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", spec.Containers[0].Args, want)
		}
	}
}

// TestApplyExtraArgsFollowReplacedArgs pins the documented precedence: a patch
// that replaces args wins, and extraArgs appends to what it left.
func TestApplyExtraArgsFollowReplacedArgs(t *testing.T) {
	workload := testWorkload("rack-a")
	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--appended"]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                args: ["--replaced"]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)

	want := []string{"--replaced", "--appended"}
	if strings.Join(spec.Containers[0].Args, ",") != strings.Join(want, ",") {
		t.Fatalf("args = %v, want %v", spec.Containers[0].Args, want)
	}
}

// TestApplyRejectsMalformedScheduling guards against a silent no-op.
//
// Scheduling is lifted out of the patch before the strategic merge. An earlier
// version removed it whether or not the type assertion succeeded, so a wrongly
// typed affinity was dropped, the override was still hashed, and the Site
// reported Applied for something that did nothing.
func TestApplyRejectsMalformedScheduling(t *testing.T) {
	cases := map[string]string{
		"affinity is a string":     "            affinity: \"everywhere\"\n",
		"tolerations is a mapping": "            tolerations:\n              key: edge\n",
		"nodeSelector is a list":   "            nodeSelector:\n              - disktype=ssd\n",
	}

	for name, fragment := range cases {
		t.Run(name, func(t *testing.T) {
			workload := testWorkload("rack-a")
			plan := planWith(workload, "storage", "rack-a")

			entries, err := parseAll(map[string]string{"overrides.yaml": doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
` + fragment)})
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			report := Apply(plan, entries, []string{"rack-a"})
			if !report.Failed() {
				t.Fatal("malformed scheduling must fail rather than silently do nothing")
			}
		})
	}
}

// TestApplyTopologySpreadIsAdditive closes a gap between two tables: conflict
// detection exempted topologySpreadConstraints as additive, but the merge left
// it to strategic merge, so two contributors sharing a topologyKey overwrote
// each other while conflict detection reported no disagreement.
func TestApplyTopologySpreadIsAdditive(t *testing.T) {
	workload := testWorkload("rack-a")

	_ = unstructured.SetNestedSlice(workload.Object, []any{
		map[string]any{
			"maxSkew":           int64(1),
			"topologyKey":       "kubernetes.io/hostname",
			"whenUnsatisfiable": "DoNotSchedule",
		},
	}, "spec", "template", "spec", "topologySpreadConstraints")

	plan := planWith(workload, "storage", "rack-a")

	constraint := func(key string) string {
		return doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            topologySpreadConstraints:
              - maxSkew: 2
                topologyKey: ` + key + `
                whenUnsatisfiable: ScheduleAnyway
`)
	}

	entries, err := parseAll(map[string]string{"a.yaml": constraint("topology.kubernetes.io/zone")})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if len(spec.TopologySpreadConstraints) != 2 {
		t.Fatalf("constraints = %d, want the operator's and the user's", len(spec.TopologySpreadConstraints))
	}
}

// TestMergeRestampsPathsATypedSiteFieldOwns is the defence-in-depth half of the
// precedence rule.
//
// Validation rejects a patch that sets an owned path, so in a running operator
// the merge never sees one. It re-stamps anyway, for the same reason identity
// is re-stamped: the typed Site field staying authoritative must not depend on
// the validator being exhaustive, and a path added to the table later is then
// protected whether or not the validator was updated to match.
//
// The entries here bypass Validate deliberately, which is the only way to
// exercise this.
func TestMergeRestampsPathsATypedSiteFieldOwns(t *testing.T) {
	workload := metalmanDeployment(2)

	entries, err := parseAll(map[string]string{"overrides.yaml": doc(`  - component: metalman
    kind: Deployment
    patch:
      spec:
        replicas: 9
        minReadySeconds: 5
`)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan := planWith(workload, "metalman", "rack-a")

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	merged := plan.Operations[0].Object

	replicas, found, err := unstructured.NestedInt64(merged.Object, "spec", "replicas")
	if err != nil || !found {
		t.Fatalf("read replicas: found=%v err=%v", found, err)
	}

	if replicas != 2 {
		t.Fatalf("replicas = %d, want the Site's 2; the typed field must survive the merge", replicas)
	}

	// The rest of the same patch still applies, so the re-stamp is scoped to
	// the owned path rather than discarding the entry.
	seconds, found, err := unstructured.NestedInt64(merged.Object, "spec", "minReadySeconds")
	if err != nil || !found || seconds != 5 {
		t.Fatalf("minReadySeconds = %d (found=%v err=%v), want the patch applied", seconds, found, err)
	}
}

// TestMergeLeavesUnownedPathsAlone confirms the re-stamp does not reach beyond
// the component that owns the path.
func TestMergeLeavesUnownedPathsAlone(t *testing.T) {
	workload := metalmanDeployment(2)
	workload.SetName("unbounded-net-controller")

	entries, err := parseAll(map[string]string{"overrides.yaml": doc(`  - component: net
    kind: Deployment
    patch:
      spec:
        replicas: 9
`)})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	plan := planWith(workload, "net", "")

	report := Apply(plan, entries, nil)
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	replicas, _, err := unstructured.NestedInt64(plan.Operations[0].Object.Object, "spec", "replicas")
	if err != nil {
		t.Fatalf("read replicas: %v", err)
	}

	if replicas != 9 {
		t.Fatalf("replicas = %d, want the override's 9; no Site field owns net's replica count", replicas)
	}
}

// metalmanDeployment builds a Deployment shaped like the one metalman plans,
// with the replica count the Site asked for.
func metalmanDeployment(replicas int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":      "metalman-controller-rack-a",
			"namespace": "unbounded-system",
		},
		"spec": map[string]any{
			"replicas": replicas,
			"selector": map[string]any{
				"matchLabels": map[string]any{"app": "unbounded-pxe"},
			},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"app": "unbounded-pxe"}},
				"spec": map[string]any{
					"containers": []any{
						map[string]any{"name": "metalman", "image": "metalman:v1"},
					},
				},
			},
		},
	}}
}
