// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// entryFrom builds a single sourced entry from a YAML fragment, so tests read
// like the documents users actually write.
func entryFrom(t *testing.T, fragment string) SourcedEntry {
	t.Helper()

	var entry Entry
	if err := yaml.UnmarshalStrict([]byte(fragment), &entry); err != nil {
		t.Fatalf("parse entry fixture: %v", err)
	}

	return SourcedEntry{Entry: entry, Source: Source{Key: "overrides.yaml", Index: 0}}
}

func validateFragment(t *testing.T, fragment string) error {
	t.Helper()

	return ValidateErr([]SourcedEntry{entryFrom(t, fragment)})
}

func TestValidateAcceptsRealisticEntries(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
	}{
		{
			name: "resources",
			fragment: `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        containers:
          - name: node
            resources:
              limits:
                memory: 512Mi
`,
		},
		{
			name: "scheduling",
			fragment: `
component: storage
kind: DaemonSet
sites: [edge-west]
patch:
  spec:
    template:
      spec:
        tolerations:
          - key: edge
            operator: Exists
        nodeSelector:
          disktype: ssd
`,
		},
		{
			name: "sidecar with explicit intent",
			fragment: `
component: gantry
kind: DaemonSet
addContainers: [log-shipper]
patch:
  spec:
    template:
      spec:
        containers:
          - name: log-shipper
            image: fluent/fluent-bit:3.1
`,
		},
		{
			name: "extra args only",
			fragment: `
component: machina
kind: Deployment
extraArgs:
  machina-controller: ["--max-concurrent-reconciles=20"]
`,
		},
		{
			name: "workload metadata and replicas",
			fragment: `
component: machina
kind: Deployment
patch:
  metadata:
    labels:
      team: platform
  spec:
    replicas: 2
`,
		},
		{
			name: "image override",
			fragment: `
component: net
kind: Deployment
patch:
  spec:
    template:
      spec:
        containers:
          - name: controller
            image: registry.example.com/net:custom
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateFragment(t, tc.fragment); err != nil {
				t.Fatalf("Validate rejected a legitimate entry: %v", err)
			}
		})
	}
}

// TestValidateRejectsGVKEscape is the highest-value test in this package.
//
// Apply is GVK-directed and the operator's ClusterRole holds escalate and bind
// on ClusterRoleBindings, so a patch that could change the object's group,
// version or kind would turn node-root into cluster-admin without touching a
// node. Validation is the first of three independent layers; the other two are
// the post-merge re-stamp and the apply-time assertion.
func TestValidateRejectsGVKEscape(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
	}{
		{
			name: "kind",
			fragment: `
component: net
kind: DaemonSet
patch:
  kind: ClusterRoleBinding
`,
		},
		{
			name: "apiVersion",
			fragment: `
component: net
kind: DaemonSet
patch:
  apiVersion: rbac.authorization.k8s.io/v1
`,
		},
		{
			name: "both",
			fragment: `
component: net
kind: DaemonSet
patch:
  apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRoleBinding
  roleRef:
    kind: ClusterRole
    name: cluster-admin
`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFragment(t, tc.fragment)
			if err == nil {
				t.Fatal("a patch able to change the object's GVK must be rejected")
			}

			if !strings.Contains(err.Error(), "escalate and bind") {
				t.Fatalf("error = %q, want it to explain the escalation risk", err)
			}
		})
	}
}

// TestValidateRejectsProtectedPaths covers the integrity controls: fields whose
// modification would sever the operator's ability to reconcile the workload.
func TestValidateRejectsProtectedPaths(t *testing.T) {
	cases := []struct {
		name     string
		patch    string
		wantPath string
	}{
		{name: "name", patch: "metadata:\n    name: renamed", wantPath: "metadata.name"},
		{name: "namespace", patch: "metadata:\n    namespace: elsewhere", wantPath: "metadata.namespace"},
		{name: "ownerReferences", patch: "metadata:\n    ownerReferences: []", wantPath: "metadata.ownerReferences"},
		{name: "finalizers", patch: "metadata:\n    finalizers: [x]", wantPath: "metadata.finalizers"},
		{name: "selector", patch: "spec:\n    selector:\n      matchLabels:\n        a: b", wantPath: "spec.selector"},
		{
			name:     "serviceAccountName",
			patch:    "spec:\n    template:\n      spec:\n        serviceAccountName: other",
			wantPath: "spec.template.spec.serviceAccountName",
		},
		{
			name:     "hostNetwork",
			patch:    "spec:\n    template:\n      spec:\n        hostNetwork: true",
			wantPath: "spec.template.spec.hostNetwork",
		},
		{
			name:     "hostPID",
			patch:    "spec:\n    template:\n      spec:\n        hostPID: true",
			wantPath: "spec.template.spec.hostPID",
		},
		{
			name:     "hostIPC",
			patch:    "spec:\n    template:\n      spec:\n        hostIPC: true",
			wantPath: "spec.template.spec.hostIPC",
		},
		{name: "status", patch: "status:\n    replicas: 9", wantPath: "status"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fragment := "component: net\nkind: DaemonSet\npatch:\n  " + tc.patch + "\n"

			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatalf("%s must be protected", tc.wantPath)
			}

			if !strings.Contains(err.Error(), tc.wantPath) || !strings.Contains(err.Error(), "is protected") {
				t.Fatalf("error = %q, want it to name %q as protected", err, tc.wantPath)
			}
		})
	}
}

// TestValidateRejectsDirectivesAtEveryDepth covers directive smuggling. An
// earlier revision of the design restricted only $patch and $setElementOrder,
// which was incomplete: the directive namespace is open.
func TestValidateRejectsDirectivesAtEveryDepth(t *testing.T) {
	cases := map[string]string{
		"top level":         "patch:\n  $patch: delete\n",
		"in spec":           "patch:\n  spec:\n    $patch: replace\n",
		"in pod spec":       "patch:\n  spec:\n    template:\n      spec:\n        $setElementOrder/containers: [a]\n",
		"inside subtree":    "patch:\n  spec:\n    template:\n      spec:\n        volumes:\n          - $patch: delete\n",
		"unknown directive": "patch:\n  spec:\n    template:\n      spec:\n        $somethingNew: x\n",
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			fragment := "component: net\nkind: DaemonSet\n" + patch

			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatal("strategic merge directives must be rejected")
			}

			if !strings.Contains(err.Error(), "directive") {
				t.Fatalf("error = %q, want it to mention directives", err)
			}
		})
	}
}

// TestValidateRejectsExplicitNulls covers the second route to removing managed
// content, which a path allowlist alone would not catch: strategic merge treats
// an explicit null as a deletion.
func TestValidateRejectsExplicitNulls(t *testing.T) {
	cases := map[string]string{
		"permitted leaf":  "patch:\n  spec:\n    replicas: null\n",
		"permitted map":   "patch:\n  spec:\n    template:\n      spec:\n        nodeSelector: null\n",
		"inside subtree":  "patch:\n  spec:\n    template:\n      spec:\n        tolerations:\n          - key: null\n",
		"container field": "patch:\n  spec:\n    template:\n      spec:\n        containers:\n          - name: node\n            image: null\n",
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			fragment := "component: net\nkind: DaemonSet\n" + patch

			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatal("an explicit null must be rejected")
			}

			if !strings.Contains(err.Error(), "null") {
				t.Fatalf("error = %q, want it to mention the null", err)
			}
		})
	}
}

// TestValidateRejectsReservedMetadataPrefix guards override visibility: a patch
// that could write the operator's own annotations could hide the fact that an
// override is in effect, or forge a config hash the reaper gates on.
func TestValidateRejectsReservedMetadataPrefix(t *testing.T) {
	cases := map[string]string{
		"workload annotations":     "patch:\n  metadata:\n    annotations:\n      unbounded-cloud.io/override-hash: forged\n",
		"workload labels":          "patch:\n  metadata:\n    labels:\n      unbounded-cloud.io/site: elsewhere\n",
		"pod template annotations": "patch:\n  spec:\n    template:\n      metadata:\n        annotations:\n          unbounded-cloud.io/net-config-hash: forged\n",
		"pod template labels":      "patch:\n  spec:\n    template:\n      metadata:\n        labels:\n          unbounded-cloud.io/site: elsewhere\n",
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			fragment := "component: net\nkind: DaemonSet\n" + patch

			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatal("the reserved prefix must be rejected")
			}

			if !strings.Contains(err.Error(), ReservedPrefix) {
				t.Fatalf("error = %q, want it to name the reserved prefix", err)
			}
		})
	}

	// A user-chosen label alongside a reserved one must still be accepted.
	if err := validateFragment(t, "component: net\nkind: DaemonSet\npatch:\n  metadata:\n    labels:\n      team: platform\n"); err != nil {
		t.Fatalf("a user-chosen label must be accepted: %v", err)
	}
}

// TestValidateRejectsUnenumeratedPaths covers the fail-closed property at the
// path level: a field nobody enumerated is denied rather than silently allowed.
func TestValidateRejectsUnenumeratedPaths(t *testing.T) {
	cases := map[string]string{
		"unknown top level":   "patch:\n  someFutureField: true\n",
		"unknown pod field":   "patch:\n  spec:\n    template:\n      spec:\n        someFutureField: true\n",
		"unknown spec field":  "patch:\n  spec:\n    someFutureField: true\n",
		"unknown container":   "patch:\n  spec:\n    template:\n      spec:\n        containers:\n          - name: node\n            someFutureField: true\n",
		"pod template spec x": "patch:\n  spec:\n    template:\n      someFutureField: true\n",
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			fragment := "component: net\nkind: DaemonSet\n" + patch

			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatal("an unenumerated path must be rejected")
			}

			if !strings.Contains(err.Error(), "not an overridable field") {
				t.Fatalf("error = %q, want it to say the field is not overridable", err)
			}
		})
	}
}

// TestValidateSubtreesAreFailOpen documents the deliberate asymmetry: within a
// permitted subtree, fields nobody enumerated are allowed, including ones added
// by future Kubernetes releases.
func TestValidateSubtreesAreFailOpen(t *testing.T) {
	fragment := `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        containers:
          - name: node
            securityContext:
              someFieldAddedInAFutureRelease: true
            resources:
              claims:
                - name: gpu
`

	if err := validateFragment(t, fragment); err != nil {
		t.Fatalf("permitted subtrees must accept unenumerated descendants: %v", err)
	}
}

func TestValidateSchema(t *testing.T) {
	cases := []struct {
		name     string
		fragment string
		want     string
	}{
		{
			name:     "missing component",
			fragment: "kind: DaemonSet\nextraArgs:\n  node: [--x]\n",
			want:     "component is required",
		},
		{
			name:     "unknown component",
			fragment: "component: nope\nkind: DaemonSet\nextraArgs:\n  node: [--x]\n",
			want:     "unknown component",
		},
		{
			name:     "missing kind",
			fragment: "component: net\nextraArgs:\n  node: [--x]\n",
			want:     "kind is required",
		},
		{
			name:     "unsupported kind",
			fragment: "component: net\nkind: Service\nextraArgs:\n  node: [--x]\n",
			want:     "unsupported kind",
		},
		{
			name:     "no work",
			fragment: "component: net\nkind: DaemonSet\n",
			want:     "changes nothing",
		},
		{
			name:     "empty sites",
			fragment: "component: storage\nkind: DaemonSet\nsites: []\nextraArgs:\n  run: [--x]\n",
			want:     "present but empty",
		},
		{
			name:     "sites on a cluster singleton",
			fragment: "component: net\nkind: DaemonSet\nsites: [edge]\nextraArgs:\n  node: [--x]\n",
			want:     "cluster singleton",
		},
		{
			name:     "duplicate site",
			fragment: "component: storage\nkind: DaemonSet\nsites: [edge, edge]\nextraArgs:\n  run: [--x]\n",
			want:     "more than once",
		},
		{
			name:     "empty site name",
			fragment: "component: storage\nkind: DaemonSet\nsites: [\"\"]\nextraArgs:\n  run: [--x]\n",
			want:     "empty Site name",
		},
		{
			name:     "duplicate addContainers",
			fragment: "component: net\nkind: DaemonSet\naddContainers: [a, a]\nextraArgs:\n  node: [--x]\n",
			want:     "more than once",
		},
		{
			name:     "empty extraArgs list",
			fragment: "component: net\nkind: DaemonSet\nextraArgs:\n  node: []\n",
			want:     "is empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFragment(t, tc.fragment)
			if err == nil {
				t.Fatalf("expected rejection mentioning %q", tc.want)
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestValidateSitesOmittedIsAccepted documents that a nil selector is the way
// to target every Site, distinct from an explicitly empty one.
func TestValidateSitesOmittedIsAccepted(t *testing.T) {
	if err := validateFragment(t, "component: storage\nkind: DaemonSet\nextraArgs:\n  run: [--x]\n"); err != nil {
		t.Fatalf("omitting sites must match every Site: %v", err)
	}
}

// TestValidateReportsEveryProblem matters for usability: a user fixing a
// document should see the whole list rather than peeling one error at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	entries := []SourcedEntry{
		{Entry: Entry{Component: "nope", Kind: "Service"}, Source: Source{Key: "a.yaml", Index: 0}},
		{Entry: Entry{Component: "net", Kind: "DaemonSet"}, Source: Source{Key: "b.yaml", Index: 3}},
	}

	err := ValidateErr(entries)
	if err == nil {
		t.Fatal("expected errors")
	}

	for _, want := range []string{"unknown component", "unsupported kind", "changes nothing", "a.yaml[0]", "b.yaml[3]"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

func TestPermittedAndProtectedPathsAreExported(t *testing.T) {
	permitted := PermittedPaths()
	if len(permitted) == 0 {
		t.Fatal("PermittedPaths returned nothing")
	}

	protected := ProtectedPaths()
	if len(protected) == 0 {
		t.Fatal("ProtectedPaths returned nothing")
	}

	// No path may be both permitted and protected, or the surface contradicts
	// itself and behavior depends on evaluation order.
	protectedSet := map[string]bool{}
	for _, path := range protected {
		protectedSet[path] = true
	}

	for _, path := range permitted {
		trimmed := strings.TrimSuffix(path, ".*")
		if protectedSet[trimmed] {
			t.Fatalf("%q is both permitted and protected", trimmed)
		}
	}
}

// TestValidateRejectsYAMLTimestamps is a regression test for a crash.
//
// yaml.v3 resolves unquoted dates to time.Time, and apimachinery's
// DeepCopyJSONValue panics on it exactly as it does on a plain int. A
// nodeSelector value such as 2026-08-11 would therefore have taken the operator
// down rather than produced an error.
//
// The timestamp is rejected rather than coerced: rewriting it to RFC3339 would
// silently produce a value the user did not write.
func TestValidateRejectsYAMLTimestamps(t *testing.T) {
	_, err := parseAll(map[string]string{"overrides.yaml": `apiVersion: ` + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              example.com/date: 2026-08-11
`})
	if err == nil {
		t.Fatal("an unquoted YAML timestamp must be rejected, not passed through to panic later")
	}

	if !strings.Contains(err.Error(), "quote it") {
		t.Fatalf("error = %q, want it to tell the user to quote the value", err)
	}

	// The quoted form is what the user meant, and must be accepted.
	if _, err := parseAll(map[string]string{"overrides.yaml": `apiVersion: ` + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              example.com/date: "2026-08-11"
`}); err != nil {
		t.Fatalf("a quoted date must be accepted: %v", err)
	}
}

// TestValidateRejectsUncomparableMergeKeys is a regression test for a crash.
//
// strategicpatch compares merge keys with Go's == operator, which panics at
// runtime on an uncomparable type. A patch containing
// `env: [{name: [oops], value: x}]` reached that comparison and crash-looped
// the operator, because containers[*].env is a permitted subtree and nothing
// checked the type of the key itself.
func TestValidateRejectsUncomparableMergeKeys(t *testing.T) {
	cases := map[string]string{
		"env name is a list": `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        containers:
          - name: node
            env:
              - name: [oops]
                value: x
`,
		"container name is a map": `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        containers:
          - name: {oops: true}
            image: x
`,
		"volumeMount mountPath is a list": `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        containers:
          - name: node
            volumeMounts:
              - name: v
                mountPath: [oops]
`,
	}

	for name, fragment := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateFragment(t, fragment)
			if err == nil {
				t.Fatal("an uncomparable merge key must be rejected before it reaches strategic merge")
			}

			if !strings.Contains(err.Error(), "strategic merge compares this value") {
				t.Fatalf("error = %q, want it to explain why the type matters", err)
			}
		})
	}
}

// TestValidateRejectsNonStringLabelValues guards against a silent data loss.
//
// unstructured's GetAnnotations returns nil for a map holding any non-string
// value, so after merging an integer annotation the operator would replace
// every annotation on the object with its own bookkeeping rather than merging
// into them.
func TestValidateRejectsNonStringLabelValues(t *testing.T) {
	cases := map[string]string{
		"integer annotation":   "patch:\n  metadata:\n    annotations:\n      revision: 3\n",
		"boolean label":        "patch:\n  metadata:\n    labels:\n      managed: true\n",
		"integer nodeSelector": "patch:\n  spec:\n    template:\n      spec:\n        nodeSelector:\n          rack: 7\n",
		"pod template label":   "patch:\n  spec:\n    template:\n      metadata:\n        labels:\n          tier: 2\n",
	}

	for name, patch := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateFragment(t, "component: net\nkind: DaemonSet\n"+patch)
			if err == nil {
				t.Fatal("a non-string label or annotation value must be rejected")
			}

			if !strings.Contains(err.Error(), "must be a string") {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

// TestValidateRejectsTheWrongShape is a regression test for a silent
// corruption.
//
// Strategic merge does not police the JSON type of a field. A patch writing
// `containers` as a mapping rather than a list, which is one missing `-` and
// the most ordinary YAML mistake there is, merged cleanly and produced an
// object whose containers field was no longer an array. The override was
// hashed, the Site reported Applied, and the apiserver rejected the workload
// with "cannot unmarshal object into Go struct field PodSpec.containers of type
// []v1.Container", which says nothing about the line the user got wrong.
func TestValidateRejectsTheWrongShape(t *testing.T) {
	cases := map[string]struct {
		patch string
		want  string
	}{
		"containers as a mapping": {
			patch: "  spec:\n    template:\n      spec:\n        containers:\n          name: agent\n          image: x\n",
			want:  "spec.template.spec.containers must be a list",
		},
		"env as a mapping": {
			patch: "  spec:\n    template:\n      spec:\n        containers:\n          - name: agent\n            env:\n              name: A\n              value: B\n",
			want:  "spec.template.spec.containers.*.env must be a list",
		},
		"args as a scalar": {
			patch: "  spec:\n    template:\n      spec:\n        containers:\n          - name: agent\n            args: --one --two\n",
			want:  "spec.template.spec.containers.*.args must be a list",
		},
		"labels as a list": {
			patch: "  metadata:\n    labels:\n      - a=b\n",
			want:  "metadata.labels must be a mapping",
		},
		"nodeSelector as a list": {
			patch: "  spec:\n    template:\n      spec:\n        nodeSelector:\n          - disktype=ssd\n",
			want:  "spec.template.spec.nodeSelector must be a mapping",
		},
		"resources as a list": {
			patch: "  spec:\n    template:\n      spec:\n        containers:\n          - name: agent\n            resources:\n              - cpu: 1\n",
			want:  "spec.template.spec.containers.*.resources must be a mapping",
		},
		"tolerations as a mapping": {
			patch: "  spec:\n    template:\n      spec:\n        tolerations:\n          key: edge\n          operator: Exists\n",
			want:  "spec.template.spec.tolerations must be a list",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateFragment(t, "component: net\nkind: DaemonSet\npatch:\n"+tc.patch)
			if err == nil {
				t.Fatal("a value of the wrong type must be rejected")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}

			if !strings.Contains(err.Error(), "indentation") {
				t.Fatalf("error = %q, want it to point at the likely cause", err)
			}
		})
	}
}

// TestValidateAcceptsTheRightShape confirms the check is not simply refusing
// everything structural.
func TestValidateAcceptsTheRightShape(t *testing.T) {
	err := validateFragment(t, `
component: net
kind: DaemonSet
patch:
  metadata:
    labels:
      team: infra
  spec:
    template:
      spec:
        nodeSelector:
          disktype: ssd
        tolerations:
          - key: edge
            operator: Exists
        containers:
          - name: node
            args:
              - --one
            env:
              - name: A
                value: B
            resources:
              requests:
                cpu: 100m
`)
	if err != nil {
		t.Fatalf("a correctly shaped patch must be accepted: %v", err)
	}
}

// TestValidateReportsOneProblemPerWrongShape checks that a shape error stops
// the walk into the value. Walking into a mapping that should have been a list
// produces a cascade of complaints about paths that only exist because the
// shape is wrong, burying the one message that matters.
func TestValidateReportsOneProblemPerWrongShape(t *testing.T) {
	err := validateFragment(t,
		"component: net\nkind: DaemonSet\npatch:\n  spec:\n    template:\n      spec:\n"+
			"        containers:\n          name: agent\n          image: x\n          nonsense: y\n")
	if err == nil {
		t.Fatal("expected a shape error")
	}

	if got := strings.Count(err.Error(), "\n"); got > 1 {
		t.Fatalf("error reports %d lines, want the single shape problem:\n%s", got, err)
	}
}

// TestValidateRejectsPathsATypedSiteFieldOwns pins the precedence between the
// two customization surfaces.
//
// Site.spec is the supported surface and an override is the escape hatch for
// what it does not cover, so where both describe the same thing the typed field
// decides. Before this, an override setting spec.replicas on metalman simply
// beat spec.components.metalman.replicas: a user editing the supported field
// would see nothing happen, with no indication why.
//
// Accepting the patch and quietly re-stamping the typed value would be no
// better. It is the silent no-op this package rejects everywhere else, and the
// user would be told the override was Applied.
func TestValidateRejectsPathsATypedSiteFieldOwns(t *testing.T) {
	err := validateFragment(t, `
component: metalman
kind: Deployment
patch:
  spec:
    replicas: 3
`)
	if err == nil {
		t.Fatal("spec.replicas on metalman must be rejected; the Site field owns it")
	}

	if !strings.Contains(err.Error(), "spec.components.metalman.replicas") {
		t.Fatalf("error = %q, want it to name the field to use instead", err)
	}
}

// TestValidateAllowsReplicasWhereNoTypedFieldOwnsIt keeps the rule narrow. Only
// paths a Site field actually sets are refused, and neither the net nor the
// machina Deployment has a typed replica count.
func TestValidateAllowsReplicasWhereNoTypedFieldOwnsIt(t *testing.T) {
	for _, component := range []string{"net", "machina"} {
		t.Run(component, func(t *testing.T) {
			err := validateFragment(t, "component: "+component+"\nkind: Deployment\npatch:\n  spec:\n    replicas: 3\n")
			if err != nil {
				t.Fatalf("spec.replicas must stay available on %s: %v", component, err)
			}
		})
	}
}

// TestValidateAllowsOtherMetalmanPaths confirms the rule is scoped to the owned
// path rather than to the component.
func TestValidateAllowsOtherMetalmanPaths(t *testing.T) {
	err := validateFragment(t, `
component: metalman
kind: Deployment
patch:
  spec:
    minReadySeconds: 5
    template:
      spec:
        containers:
          - name: metalman
            imagePullPolicy: Always
`)
	if err != nil {
		t.Fatalf("only the owned path is refused: %v", err)
	}
}

// TestValidateRejectsKindsAComponentNeverEmits is a regression test.
//
// Component and kind were validated against separate lists, so seven of the ten
// pairs they accepted between them could not resolve to anything: machina emits
// no DaemonSet, gantry no Deployment. Such an entry validated, matched nothing,
// and the ConfigMap received a success Event saying zero workloads were
// overridden. Naming the mistake is the whole job of validation.
func TestValidateRejectsKindsAComponentNeverEmits(t *testing.T) {
	impossible := map[string]string{
		"machina has no DaemonSet":  "component: machina\nkind: DaemonSet\n",
		"gantry has no Deployment":  "component: gantry\nkind: Deployment\n",
		"metalman has no DaemonSet": "component: metalman\nkind: DaemonSet\nsites: [edge]\n",
		"storage has no Deployment": "component: storage\nkind: Deployment\nsites: [edge]\n",
	}

	for name, header := range impossible {
		t.Run(name, func(t *testing.T) {
			err := validateFragment(t, header+"patch:\n  spec:\n    minReadySeconds: 5\n")
			if err == nil {
				t.Fatal("an entry that can never match anything must be rejected")
			}

			if !strings.Contains(err.Error(), "can never match") {
				t.Fatalf("error = %q, want it to say the entry cannot match", err)
			}
		})
	}
}

// TestValidateAcceptsKindsAComponentDoesEmit keeps the rule from being a
// blanket refusal. net is the one component emitting both kinds.
func TestValidateAcceptsKindsAComponentDoesEmit(t *testing.T) {
	for _, header := range []string{
		"component: net\nkind: Deployment\n",
		"component: net\nkind: DaemonSet\n",
		"component: machina\nkind: Deployment\n",
		"component: gantry\nkind: DaemonSet\n",
		"component: storage\nkind: DaemonSet\nsites: [edge]\n",
	} {
		if err := validateFragment(t, header+"patch:\n  spec:\n    minReadySeconds: 5\n"); err != nil {
			t.Fatalf("%s must be accepted: %v", header, err)
		}
	}
}

// TestValidateRejectsMalformedAffinityExpressions is a regression test.
//
// affinity is a permitted subtree, so the allowlist walker does not descend
// into it and no shape check applied. Combining terms then asserted these were
// lists and discarded the failure, so the user's constraint vanished while the
// override was hashed and reported Applied. A term whose only field was
// malformed became an empty term, which matches every node, so a constraint
// meant to narrow scheduling widened it instead.
func TestValidateRejectsMalformedAffinityExpressions(t *testing.T) {
	fragment := func(expressions string) string {
		return `
component: net
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        affinity:
          nodeAffinity:
            requiredDuringSchedulingIgnoredDuringExecution:
              nodeSelectorTerms:
                - ` + expressions + `
`
	}

	cases := map[string]string{
		"matchExpressions as a mapping": "matchExpressions:\n                    key: disktype",
		"matchExpressions as a scalar":  "matchExpressions: disktype",
		"matchFields as a scalar":       "matchFields: nope",
		"expression entry not a map":    "matchExpressions:\n                    - disktype=ssd",
	}

	for name, expressions := range cases {
		t.Run(name, func(t *testing.T) {
			err := validateFragment(t, fragment(expressions))
			if err == nil {
				t.Fatal("a malformed affinity expression must be rejected, not silently discarded")
			}
		})
	}

	// The well-formed shape is still accepted.
	if err := validateFragment(t, fragment(
		"matchExpressions:\n                    - key: disktype\n                      operator: In\n                      values: [ssd]",
	)); err != nil {
		t.Fatalf("a well-formed term must be accepted: %v", err)
	}
}

// TestValidateRejectsUndeliverableContainerAdditions is a regression test.
//
// A name in addContainers with no matching container in the patch creates
// nothing, and extraArgs targeting it was accepted too, because extraArgs
// validates against declarations rather than definitions. The entry passed
// validation, passed resolution, merged cleanly, was hashed, and did nothing.
func TestValidateRejectsUndeliverableContainerAdditions(t *testing.T) {
	err := validateFragment(t, `
component: gantry
kind: DaemonSet
addContainers: [log-shipper]
extraArgs:
  log-shipper: ["--verbose"]
`)
	if err == nil {
		t.Fatal("a declared addition with no definition must be rejected")
	}

	if !strings.Contains(err.Error(), "nothing would be created") {
		t.Fatalf("error = %q, want it to say the container is never created", err)
	}
}

// TestValidateRejectsNameInBothContainerLists covers the collision Kubernetes
// itself rejects. Each list was checked for duplicates on its own, so a name in
// both was never seen and the apiserver refused the pod after the override had
// been applied and reported.
func TestValidateRejectsNameInBothContainerLists(t *testing.T) {
	err := validateFragment(t, `
component: gantry
kind: DaemonSet
addContainers: [helper]
addInitContainers: [helper]
patch:
  spec:
    template:
      spec:
        containers:
          - name: helper
            image: busybox
        initContainers:
          - name: helper
            image: busybox
`)
	if err == nil {
		t.Fatal("a name in both container lists must be rejected")
	}

	if !strings.Contains(err.Error(), "unique across both lists") {
		t.Fatalf("error = %q, want it to explain the Kubernetes constraint", err)
	}
}

// TestValidateRejectsMalformedAffinityExtras pins the offline half of the
// malformed-affinity fix.
//
// affinity is a permitted subtree, so the allowlist walker does not descend
// into it and no shape check applied. Only required node affinity was checked,
// which left every preference and both pod-affinity sections able to reach the
// merge in a shape the merge would silently drop.
func TestValidateRejectsMalformedAffinityExtras(t *testing.T) {
	fragment := func(parent, section string) string {
		return `
component: storage
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        affinity:
          ` + parent + `:
            ` + section + `:
              weight: 1
`
	}

	for _, tc := range []struct {
		name    string
		parent  string
		section string
	}{
		{name: "node preference", parent: "nodeAffinity", section: "preferredDuringSchedulingIgnoredDuringExecution"},
		{name: "pod affinity", parent: "podAffinity", section: "requiredDuringSchedulingIgnoredDuringExecution"},
		{name: "pod anti-affinity", parent: "podAntiAffinity", section: "preferredDuringSchedulingIgnoredDuringExecution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateFragment(t, fragment(tc.parent, tc.section))
			if err == nil {
				t.Fatal("a mapping where Kubernetes requires a list must be rejected")
			}

			if !strings.Contains(err.Error(), tc.section) {
				t.Fatalf("error = %v, want it to name %s", err, tc.section)
			}
		})
	}
}

// TestValidateAcceptsWellFormedAffinityExtras guards against the shape check
// rejecting the shape it exists to permit.
func TestValidateAcceptsWellFormedAffinityExtras(t *testing.T) {
	if err := validateFragment(t, `
component: storage
kind: DaemonSet
patch:
  spec:
    template:
      spec:
        affinity:
          nodeAffinity:
            preferredDuringSchedulingIgnoredDuringExecution:
              - weight: 1
                preference:
                  matchExpressions:
                    - key: disk
                      operator: In
                      values: [ssd]
`); err != nil {
		t.Fatalf("a well-formed preference must be accepted: %v", err)
	}
}
