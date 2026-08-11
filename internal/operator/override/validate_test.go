// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

	return Validate([]SourcedEntry{entryFrom(t, fragment)})
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

	err := Validate(entries)
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
	// itself and behaviour depends on evaluation order.
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
