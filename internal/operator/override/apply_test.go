// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Azure/unbounded/internal/operator/component"
)

// multiSitePlan builds a plan with one storage DaemonSet per Site plus a
// cluster-singleton net DaemonSet, so Site selection can be exercised.
func multiSitePlan(sites ...string) *component.Plan {
	plan := component.NewPlan()

	for _, site := range sites {
		plan.Add(component.Operation{
			Kind:        component.OpApply,
			Object:      testWorkload(site),
			Component:   "storage",
			Site:        site,
			Overridable: true,
		})
	}

	netNode := testWorkload("cluster")
	netNode.SetName("unbounded-net-node")

	plan.Add(component.Operation{
		Kind:        component.OpApply,
		Object:      netNode,
		Component:   "net",
		Overridable: true,
	})

	// A non-overridable operation must never be a merge candidate.
	rbac := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "storage"},
	}}

	plan.Add(component.Operation{Kind: component.OpApply, Object: rbac, Component: "storage"})

	return plan
}

func TestResolveSelectsSites(t *testing.T) {
	plan := multiSitePlan("rack-a", "rack-b", "rack-c")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    sites: [rack-a, rack-c]
    extraArgs:
      run: ["--selected"]
`))

	resolution := Resolve(plan, entries, []string{"rack-a", "rack-b", "rack-c"})
	if len(resolution.Targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(resolution.Targets))
	}

	for _, target := range resolution.Targets {
		if strings.Contains(target.Ref.Name, "rack-b") {
			t.Fatalf("rack-b was selected but not listed: %s", target.Ref)
		}
	}
}

// TestResolveOmittedSitesMatchesEverySite covers the documented distinction
// between an absent selector and an empty one.
func TestResolveOmittedSitesMatchesEverySite(t *testing.T) {
	plan := multiSitePlan("rack-a", "rack-b")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--all"]
`))

	resolution := Resolve(plan, entries, []string{"rack-a", "rack-b"})
	if len(resolution.Targets) != 2 {
		t.Fatalf("targets = %d, want every Site", len(resolution.Targets))
	}
}

// TestResolveIgnoresNonOverridableOperations is what confines overrides to the
// workloads the operator generates: RBAC, Services and ConfigMaps are never
// merge candidates.
func TestResolveIgnoresNonOverridableOperations(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := []SourcedEntry{{
		Source: Source{Key: "a.yaml", Index: 0},
		Entry:  Entry{Component: "storage", Kind: "ClusterRole", ExtraArgs: map[string][]string{"x": {"--y"}}},
	}}

	resolution := Resolve(plan, entries, []string{"rack-a"})
	if len(resolution.Targets) != 0 {
		t.Fatalf("targets = %d, want 0; only overridable operations may be targeted", len(resolution.Targets))
	}
}

// TestResolveReportsUnmatchedSitesWithoutFailing covers the deliberate choice
// that a document may be written before its Site exists, and that deleting a
// Site must not retroactively invalidate an unrelated override.
func TestResolveReportsUnmatchedSitesWithoutFailing(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    sites: [rack-a, not-yet-created]
    extraArgs:
      run: ["--x"]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("an unmatched Site name must not fail the document: %v", report.Err())
	}

	if len(report.UnmatchedSites) != 1 || report.UnmatchedSites[0] != "not-yet-created" {
		t.Fatalf("unmatched = %v, want [not-yet-created]", report.UnmatchedSites)
	}
}

// TestApplyRejectsMisspelledContainer is the add-versus-modify distinction.
//
// Strategic merge cannot tell a sidecar from a typo, so without explicit intent
// this patch would add an image-less container named machina-contoller and
// leave the real limit untouched.
func TestApplyRejectsMisspelledContainer(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: rnu
                resources:
                  limits:
                    memory: 512Mi
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("a patch naming a container that does not exist must fail")
	}

	if !strings.Contains(report.Err().Error(), "addContainers") {
		t.Fatalf("error = %v, want it to point at addContainers", report.Err())
	}
}

// TestApplyAcceptsDeclaredSidecar is the other half: an addition works when the
// intent is explicit.
func TestApplyAcceptsDeclaredSidecar(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    addContainers: [log-shipper]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: log-shipper
                image: fluent/fluent-bit:3.1
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("a declared sidecar must be accepted: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if len(spec.Containers) != 2 {
		t.Fatalf("containers = %d, want the operator's plus the sidecar", len(spec.Containers))
	}
}

// TestApplyRejectsAddingAnExistingContainer covers the inverse mistake.
func TestApplyRejectsAddingAnExistingContainer(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    addContainers: [run]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                image: other:v1
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("declaring an existing container as an addition must fail")
	}

	if !strings.Contains(report.Err().Error(), "already has one with that name") {
		t.Fatalf("error = %v", report.Err())
	}
}

// TestApplyRejectsMountPathCollision covers the bypass that protecting mounts
// by name would have allowed: volumeMounts merge on mountPath, so a differently
// named mount on a colliding path repoints an operator-managed mount.
func TestApplyRejectsMountPathCollision(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                volumeMounts:
                  - name: attacker-volume
                    mountPath: /etc/storage
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("repointing an operator mount by colliding on mountPath must fail")
	}

	if !strings.Contains(report.Err().Error(), "merge on mountPath") {
		t.Fatalf("error = %v, want it to explain the merge key", report.Err())
	}
}

// TestApplyAcceptsNewMountPath confirms the check is targeted rather than a
// blanket ban on adding mounts.
func TestApplyAcceptsNewMountPath(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            volumes:
              - name: extra
                emptyDir: {}
            containers:
              - name: run
                volumeMounts:
                  - name: extra
                    mountPath: /var/extra
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("adding a mount at a fresh path must be accepted: %v", report.Err())
	}
}

// TestApplyComposesDisjointContributors covers the ownership-split use case:
// two teams owning separate ConfigMap keys must compose without conflict.
func TestApplyComposesDisjointContributors(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries, err := parseAll(map[string]string{
		"resources.yaml": doc(`  - component: storage
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
`),
		"scheduling.yaml": doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            tolerations:
              - key: edge
                operator: Exists
`),
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if err := ValidateErr(entries); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("disjoint contributors must compose: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if spec.Containers[0].Resources.Limits.Memory().String() != "512Mi" {
		t.Fatal("resources contributor was lost")
	}

	if len(spec.Tolerations) != 1 {
		t.Fatalf("tolerations = %+v, want the scheduling contributor", spec.Tolerations)
	}
}

// TestApplyIdenticalValuesDoNotConflict matters for the ownership-split case:
// two teams independently setting the same limit is not an error, and failing
// it would make the case unusable.
func TestApplyIdenticalValuesDoNotConflict(t *testing.T) {
	plan := multiSitePlan("rack-a")

	sameLimit := doc(`  - component: storage
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
`)

	entries, err := parseAll(map[string]string{"a.yaml": sameLimit, "b.yaml": sameLimit})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("identical values must not conflict: %v", report.Err())
	}
}

// TestApplyRejectsTrueConflict covers the opposite: where composition would be
// order-dependent, the result is rejected rather than resolved by ConfigMap key
// ordering.
func TestApplyRejectsTrueConflict(t *testing.T) {
	plan := multiSitePlan("rack-a")

	limit := func(memory string) string {
		return doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                resources:
                  limits:
                    memory: ` + memory + "\n")
	}

	entries, err := parseAll(map[string]string{"a.yaml": limit("512Mi"), "b.yaml": limit("1Gi")})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("two contributors setting one leaf to different values must conflict")
	}

	message := report.Err().Error()
	for _, want := range []string{"a.yaml[0]", "b.yaml[0]", "do not resolve disagreement by ordering"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error = %q, want it to contain %q", message, want)
		}
	}
}

// TestApplyConflictIsScopedToOneObject covers the partial-overlap case: two
// entries selecting [rack-a, rack-b] and [rack-b, rack-c] conflict only on
// rack-b, and the other two Sites must still reconcile.
func TestApplyConflictIsScopedToOneObject(t *testing.T) {
	plan := multiSitePlan("rack-a", "rack-b", "rack-c")

	entries, err := parseAll(map[string]string{
		"a.yaml": doc(`  - component: storage
    kind: DaemonSet
    sites: [rack-a, rack-b]
    extraArgs:
      run: ["--from-a"]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                image: image-a
`),
		"b.yaml": doc(`  - component: storage
    kind: DaemonSet
    sites: [rack-b, rack-c]
    extraArgs:
      run: ["--from-b"]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                image: image-b
`),
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	before := len(plan.Operations)

	report := Apply(plan, entries, []string{"rack-a", "rack-b", "rack-c"})
	if !report.Failed() {
		t.Fatal("rack-b has two contributors setting different images and must conflict")
	}

	// Exactly one operation is dropped, and it is rack-b's.
	if got := len(plan.Operations); got != before-1 {
		t.Fatalf("operations = %d, want %d; only the conflicting object may be dropped", got, before-1)
	}

	for _, op := range plan.Operations {
		if strings.Contains(op.Object.GetName(), "rack-b") {
			t.Fatal("the conflicting rack-b workload must be dropped from the plan")
		}
	}

	// rack-a and rack-c must have been merged normally.
	var merged int

	for _, op := range plan.Operations {
		if op.Object.GetAnnotations()[HashAnnotation] != "" {
			merged++
		}
	}

	if merged != 2 {
		t.Fatalf("merged workloads = %d, want rack-a and rack-c", merged)
	}
}

// TestApplyDropsRatherThanRevertsOnFailure is the failure-scoping guarantee: a
// workload whose overrides cannot be applied is left alone rather than written
// without them, so a mistake does not rewrite running infrastructure.
func TestApplyDropsRatherThanRevertsOnFailure(t *testing.T) {
	plan := multiSitePlan("rack-a")
	before := len(plan.Operations)

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: typo
                image: x
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("expected the resolution failure")
	}

	if got := len(plan.Operations); got != before-1 {
		t.Fatalf("operations = %d, want %d", got, before-1)
	}

	for _, op := range plan.Operations {
		if op.Overridable && op.Component == "storage" {
			t.Fatal("the failed workload must be dropped, not applied un-overridden")
		}
	}
}

// TestApplyStampsAnnotations covers the observability contract: an overridden
// workload names its contributors and its hash.
func TestApplyStampsAnnotations(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--x"]
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	annotations := plan.Operations[0].Object.GetAnnotations()
	if annotations[HashAnnotation] == "" {
		t.Fatal("override hash was not stamped")
	}

	if annotations[SourceAnnotation] != "overrides.yaml[0]" {
		t.Fatalf("source = %q, want overrides.yaml[0]", annotations[SourceAnnotation])
	}

	if annotations[VersionDriftAnnotation] != "" {
		t.Fatalf("version drift = %q, want none for an args-only override", annotations[VersionDriftAnnotation])
	}
}

// TestApplyReportsImageDrift covers the loudest signal in the design: a pinned
// image survives operator upgrades and is the likeliest cause of an install
// behaving unlike its reported version.
func TestApplyReportsImageDrift(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            containers:
              - name: run
                image: registry.example.com/storage:pinned
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	if report.Workloads[0].VersionDrift != "run=registry.example.com/storage:pinned" {
		t.Fatalf("drift = %q", report.Workloads[0].VersionDrift)
	}

	if got := plan.Operations[0].Object.GetAnnotations()[VersionDriftAnnotation]; got == "" {
		t.Fatal("version drift must be visible on the workload itself")
	}
}

// TestApplyHashesAreComparablePerWorkload is a regression test for a specific
// defect: hashing a per-object applied set against a desired hash covering the
// whole ConfigMap made the two differ whenever a document targeted more than
// one workload, so the divergence signal was permanently on.
func TestApplyHashesAreComparablePerWorkload(t *testing.T) {
	plan := multiSitePlan("rack-a", "rack-b")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--all"]
`))

	report := Apply(plan, entries, []string{"rack-a", "rack-b"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	if len(report.Workloads) != 2 {
		t.Fatalf("workloads = %d, want 2", len(report.Workloads))
	}

	// Both workloads have the same single contributor, so their hashes match
	// each other and the annotation each carries.
	for i, workload := range report.Workloads {
		if workload.Hash == "" {
			t.Fatalf("workload %d has no hash", i)
		}

		if workload.Hash != report.Workloads[0].Hash {
			t.Fatal("identical contributor sets must hash identically")
		}
	}

	for _, op := range plan.Operations {
		if !op.Overridable || op.Component != "storage" {
			continue
		}

		if op.Object.GetAnnotations()[HashAnnotation] != report.Workloads[0].Hash {
			t.Fatal("the annotation and the reported hash must agree")
		}
	}
}

// TestApplyHashChangesWithContent confirms the hash is sensitive to what it
// covers, or divergence would never be detected.
func TestApplyHashChangesWithContent(t *testing.T) {
	hashFor := func(arg string) string {
		plan := multiSitePlan("rack-a")

		entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["`+arg+`"]
`))

		report := Apply(plan, entries, []string{"rack-a"})
		if report.Failed() {
			t.Fatalf("Apply: %v", report.Err())
		}

		return report.Workloads[0].Hash
	}

	if hashFor("--one") == hashFor("--two") {
		t.Fatal("different override content must hash differently")
	}
}

// TestApplyAddContainerConflicts covers the one conflict rule that is not a
// leaf comparison: two contributors may add the same sidecar, but only if they
// describe it identically.
func TestApplyAddContainerConflicts(t *testing.T) {
	sidecar := func(image string) string {
		return doc(`  - component: storage
    kind: DaemonSet
    addContainers: [log-shipper]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: log-shipper
                image: ` + image + "\n")
	}

	t.Run("identical additions compose", func(t *testing.T) {
		plan := multiSitePlan("rack-a")

		entries, err := parseAll(map[string]string{"a.yaml": sidecar("fluent:1"), "b.yaml": sidecar("fluent:1")})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if report := Apply(plan, entries, []string{"rack-a"}); report.Failed() {
			t.Fatalf("identical additions must compose: %v", report.Err())
		}
	})

	t.Run("differing additions conflict", func(t *testing.T) {
		plan := multiSitePlan("rack-a")

		entries, err := parseAll(map[string]string{"a.yaml": sidecar("fluent:1"), "b.yaml": sidecar("fluent:2")})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		report := Apply(plan, entries, []string{"rack-a"})
		if !report.Failed() {
			t.Fatal("two contributors adding one container differently must conflict")
		}

		if !strings.Contains(report.Err().Error(), "different definitions") {
			t.Fatalf("error = %v", report.Err())
		}
	})
}

// TestApplyInitContainers covers the separate add list, which exists because
// containers and initContainers are separate merge-keyed lists.
func TestApplyInitContainers(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    addInitContainers: [setup]
    patch:
      spec:
        template:
          spec:
            initContainers:
              - name: setup
                image: busybox:1
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("a declared init container must be accepted: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)
	if len(spec.InitContainers) != 1 || spec.InitContainers[0].Name != "setup" {
		t.Fatalf("initContainers = %+v", spec.InitContainers)
	}
}

// TestApplyRejectsMisspelledInitContainer confirms the modify-only default
// applies to initContainers too, using the right field name in the message.
func TestApplyRejectsMisspelledInitContainer(t *testing.T) {
	plan := multiSitePlan("rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            initContainers:
              - name: nonexistent
                image: busybox:1
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if !report.Failed() {
		t.Fatal("a patch naming an init container that does not exist must fail")
	}

	if !strings.Contains(report.Err().Error(), "addInitContainers") {
		t.Fatalf("error = %v, want it to name addInitContainers", report.Err())
	}
}

// TestApplyPreservesPodAntiAffinity covers the affinity sections that are
// concatenated rather than multiplied, since only required node affinity is a
// hard constraint the operator relies on.
func TestApplyPreservesPodAntiAffinity(t *testing.T) {
	workload := testWorkload("rack-a")

	affinity, _, _ := unstructured.NestedMap(workload.Object, "spec", "template", "spec", "affinity")
	affinity["podAntiAffinity"] = map[string]any{
		"preferredDuringSchedulingIgnoredDuringExecution": []any{
			map[string]any{"weight": int64(100)},
		},
	}
	_ = unstructured.SetNestedMap(workload.Object, affinity, "spec", "template", "spec", "affinity")

	plan := planWith(workload, "storage", "rack-a")

	entries := entriesFrom(t, doc(`  - component: storage
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            affinity:
              podAntiAffinity:
                preferredDuringSchedulingIgnoredDuringExecution:
                  - weight: 50
`))

	report := Apply(plan, entries, []string{"rack-a"})
	if report.Failed() {
		t.Fatalf("Apply: %v", report.Err())
	}

	spec := podSpec(t, plan.Operations[0].Object)

	got := spec.Affinity.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution
	if len(got) != 2 {
		t.Fatalf("pod anti-affinity = %d entries, want the operator's and the user's", len(got))
	}

	// The Site constraint must be untouched by an unrelated affinity section.
	terms := spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 2 {
		t.Fatalf("node affinity terms = %d, want the operator's two", len(terms))
	}
}

// TestApplyRejectsTwoContributorsAppendingToOneContainer pins that extraArgs
// does not resolve disagreement by ConfigMap key ordering.
//
// Unlike a patch leaf, extraArgs concatenates, so two contributors do not
// overwrite one another: both lists land, ordered by sorted ConfigMap key. Two
// teams appending --log-level=debug and --log-level=warn both got their way,
// and which one the component honoured was decided by its own flag parsing.
// That is exactly the silent precedence the deterministic ordering exists to
// avoid rather than to provide.
func TestApplyRejectsTwoContributorsAppendingToOneContainer(t *testing.T) {
	args := func(value string) string {
		return `apiVersion: ` + APIVersion + `
overrides:
  - component: storage
    kind: DaemonSet
    extraArgs:
      run: ["--log-level=` + value + `"]
`
	}

	t.Run("differing arguments conflict", func(t *testing.T) {
		plan := multiSitePlan("rack-a")

		entries, err := parseAll(map[string]string{"a.yaml": args("debug"), "b.yaml": args("warn")})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		report := Apply(plan, entries, []string{"rack-a"})
		if !report.Failed() {
			t.Fatal("two contributors appending to one container must conflict")
		}

		if !strings.Contains(report.Err().Error(), "both append extraArgs to container") {
			t.Fatalf("error = %v", report.Err())
		}
	})

	// Identical lists conflict too, which is where this differs from the
	// identical-values rule for patch leaves. Setting one value twice is the
	// same as setting it once; appending the same argument twice is not.
	t.Run("identical arguments also conflict", func(t *testing.T) {
		plan := multiSitePlan("rack-a")

		entries, err := parseAll(map[string]string{"a.yaml": args("debug"), "b.yaml": args("debug")})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if report := Apply(plan, entries, []string{"rack-a"}); !report.Failed() {
			t.Fatal("appending the same argument twice is not idempotent and must conflict")
		}
	})

	// The rule is per container, not per workload: splitting arguments across
	// keys by container is exactly the ownership split the format exists for.
	t.Run("different containers compose", func(t *testing.T) {
		plan := multiSitePlan("rack-a")

		sidecar := `apiVersion: ` + APIVersion + `
overrides:
  - component: storage
    kind: DaemonSet
    addContainers: [log-shipper]
    extraArgs:
      log-shipper: ["--verbose"]
    patch:
      spec:
        template:
          spec:
            containers:
              - name: log-shipper
                image: fluent/fluent-bit:3.1
`

		entries, err := parseAll(map[string]string{"a.yaml": args("debug"), "b.yaml": sidecar})
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if report := Apply(plan, entries, []string{"rack-a"}); report.Failed() {
			t.Fatalf("entries targeting different containers must compose: %v", report.Err())
		}
	})
}
