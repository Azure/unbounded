// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package override

import (
	"fmt"
	"strings"
	"testing"
)

const validDocument = `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: net
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
`

func TestParseValidDocument(t *testing.T) {
	entries, err := parseAll(map[string]string{"overrides.yaml": validDocument})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}

	if got := entries[0].Source.String(); got != "overrides.yaml[0]" {
		t.Fatalf("source = %q, want overrides.yaml[0]", got)
	}

	if entries[0].Entry.Component != "net" || entries[0].Entry.Kind != "DaemonSet" {
		t.Fatalf("entry = %+v", entries[0].Entry)
	}
}

// TestParseIsDeterministic covers the ordering composition depends on: sorted
// ConfigMap key, then position within that key's document.
func TestParseIsDeterministic(t *testing.T) {
	two := `apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: ["--first"]
  - component: net
    kind: Deployment
    extraArgs:
      controller: ["--second"]
`

	data := map[string]string{"zulu.yaml": two, "alpha.yaml": validDocument}

	for range 5 {
		entries, err := parseAll(data)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		want := []string{"alpha.yaml[0]", "zulu.yaml[0]", "zulu.yaml[1]"}
		if len(entries) != len(want) {
			t.Fatalf("entries = %d, want %d", len(entries), len(want))
		}

		for i, source := range want {
			if got := entries[i].Source.String(); got != source {
				t.Fatalf("entry %d source = %q, want %q", i, got, source)
			}
		}
	}
}

// TestParseIgnoresEmptyKeys covers `kubectl create configmap --from-file` on an
// empty file, which is an accident rather than an intent.
func TestParseIgnoresEmptyKeys(t *testing.T) {
	entries, err := parseAll(map[string]string{
		"empty.yaml":     "",
		"whitespace.yml": "   \n\t\n",
		"real.yaml":      validDocument,
	})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
}

// TestParseRejects covers the parsing rules the apiVersion gate depends on. If
// a document that passes the gate is not the document the user wrote, the
// versioning promise is worthless.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			name: "missing apiVersion",
			doc:  "overrides: []\n",
			want: "apiVersion is required",
		},
		{
			name: "unknown apiVersion",
			doc:  "apiVersion: overrides.unbounded-cloud.io/v9\noverrides: []\n",
			want: "unsupported apiVersion",
		},
		{
			name: "unknown top-level field",
			doc:  "apiVersion: " + APIVersion + "\nbogus: true\noverrides: []\n",
			want: "field bogus not found",
		},
		{
			name: "unknown entry field",
			doc: "apiVersion: " + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    bogusField: true
`,
			want: "field bogusField not found",
		},
		{
			name: "duplicate key",
			doc:  "apiVersion: " + APIVersion + "\napiVersion: " + APIVersion + "\noverrides: []\n",
			want: "already defined",
		},
		{
			name: "second document",
			doc:  "apiVersion: " + APIVersion + "\noverrides: []\n---\napiVersion: " + APIVersion + "\noverrides: []\n",
			want: "more than one YAML document",
		},
		{
			name: "trailing content after separator",
			doc:  "apiVersion: " + APIVersion + "\noverrides: []\n---\nnonsense\n",
			want: "more than one YAML document",
		},
		{
			name: "merge key",
			doc: "apiVersion: " + APIVersion + `
base: &base
  component: net
overrides:
  - <<: *base
    kind: DaemonSet
`,
			want: "merge keys",
		},
		{
			name: "malformed yaml",
			doc:  "apiVersion: [unclosed\n",
			want: "overrides key",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAll(map[string]string{"overrides.yaml": tc.doc})
			if err == nil {
				t.Fatal("expected the document to be rejected")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestParseErrorsNameTheKey matters because a ConfigMap can hold many
// documents and the user needs to know which one is wrong.
func TestParseErrorsNameTheKey(t *testing.T) {
	_, err := parseAll(map[string]string{
		"good.yaml": validDocument,
		"bad.yaml":  "overrides: []\n",
	})
	if err == nil {
		t.Fatal("expected an error")
	}

	if !strings.Contains(err.Error(), `"bad.yaml"`) {
		t.Fatalf("error = %q, want it to name bad.yaml", err)
	}
}

// TestParseNormalizesIntegers is a regression test for a crash.
//
// yaml.v3 decodes whole numbers as int, but apimachinery's DeepCopyJSONValue
// accepts only int64 and panics on anything else. Before normalization the
// first user to write spec.replicas, a container port, or an affinity weight
// would crash the operator rather than get an error.
func TestParseNormalizesIntegers(t *testing.T) {
	entries, err := parseAll(map[string]string{"overrides.yaml": `apiVersion: ` + APIVersion + `
overrides:
  - component: machina
    kind: Deployment
    patch:
      spec:
        replicas: 2
        template:
          spec:
            terminationGracePeriodSeconds: 30
            containers:
              - name: machina-controller
                ports:
                  - containerPort: 8080
`})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	patch := entries[0].Entry.Patch

	if got, ok := patch["spec"].(map[string]any)["replicas"].(int64); !ok || got != 2 {
		t.Fatalf("replicas = %#v, want int64(2)", patch["spec"].(map[string]any)["replicas"])
	}

	// Every numeric value anywhere in the tree must be int64, or the
	// unstructured helpers panic when the merge touches it.
	assertNoPlainInts(t, patch, "patch")
}

func assertNoPlainInts(t *testing.T, value any, path string) {
	t.Helper()

	switch typed := value.(type) {
	case map[string]any:
		for key, element := range typed {
			assertNoPlainInts(t, element, path+"."+key)
		}
	case []any:
		for i, element := range typed {
			assertNoPlainInts(t, element, path)

			_ = i
		}
	case int, int8, int16, int32, uint, uint8, uint16, uint32, uint64, float32:
		t.Fatalf("%s holds %T, which apimachinery cannot deep copy", path, value)
	}
}

// TestParseRejectsDocumentsThatWouldStallTheOperator is a regression test for a
// denial of service reachable with an entirely legal document.
//
// yaml.v3 checks for duplicate mapping keys with a nested loop over every pair,
// on by default, so decoding is quadratic in the number of keys in one mapping.
// The operator reconciles with a single worker and re-parses on every pass.
// Measured with yaml.v3 v3.0.1, one mapping of 40,000 annotation keys in 1.2 MB
// took 4.1 seconds to decode; the apiserver's own ~1 MiB ConfigMap limit is
// therefore not a bound on the work.
//
// An earlier revision of the design argued no size limit was needed for exactly
// that reason. It assumed parsing cost was linear in the payload.
func TestParseRejectsDocumentsThatWouldStallTheOperator(t *testing.T) {
	annotations := func(n int) string {
		var b strings.Builder

		b.WriteString("apiVersion: " + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    patch:
      metadata:
        annotations:
`)

		for i := range n {
			fmt.Fprintf(&b, "          example.com/k%d: v\n", i)
		}

		return b.String()
	}

	// Comfortably under the limit: still accepted, so the cap is not simply
	// refusing realistic documents.
	if _, err := parseAll(map[string]string{"overrides.yaml": annotations(200)}); err != nil {
		t.Fatalf("a 200-key mapping must be accepted: %v", err)
	}

	_, err := parseAll(map[string]string{"overrides.yaml": annotations(maxMappingKeys + 1)})
	if err == nil {
		t.Fatal("a mapping over the key limit must be rejected before the quadratic decode runs")
	}

	if !strings.Contains(err.Error(), "quadratic") {
		t.Fatalf("error = %q, want it to say why the limit exists", err)
	}
}

// TestParseRejectsOversizedInput covers the byte and entry limits, which bound
// the work that many small mappings can add up to.
func TestParseRejectsOversizedInput(t *testing.T) {
	entry := `  - component: net
    kind: DaemonSet
    patch:
      spec:
        minReadySeconds: 1
`

	t.Run("one document over the byte limit", func(t *testing.T) {
		doc := "apiVersion: " + APIVersion + "\noverrides:\n" +
			strings.Repeat(entry, 1+maxDocumentBytes/len(entry))

		if _, err := parseAll(map[string]string{"overrides.yaml": doc}); err == nil {
			t.Fatal("an oversized document must be rejected")
		}
	})

	t.Run("keys that are individually fine but oversized together", func(t *testing.T) {
		one := "apiVersion: " + APIVersion + "\noverrides:\n" +
			strings.Repeat(entry, (maxDocumentBytes/len(entry))-10)

		data := map[string]string{}
		for i := range 1 + maxTotalBytes/len(one) {
			data[fmt.Sprintf("part-%02d.yaml", i)] = one
		}

		if _, err := parseAll(data); err == nil {
			t.Fatal("keys under the per-document limit must still be bounded in total")
		}
	})

	t.Run("too many entries", func(t *testing.T) {
		doc := "apiVersion: " + APIVersion + "\noverrides:\n" + strings.Repeat(entry, maxEntries+1)

		if _, err := parseAll(map[string]string{"overrides.yaml": doc}); err == nil {
			t.Fatal("a document over the entry limit must be rejected")
		}
	})
}

// TestParseRejectsExcessiveNesting bounds the depth every recursive walk in
// this package descends.
func TestParseRejectsExcessiveNesting(t *testing.T) {
	var b strings.Builder

	b.WriteString("apiVersion: " + APIVersion + "\noverrides:\n  - component: net\n    kind: DaemonSet\n    patch:\n")

	for i := range maxDepth + 4 {
		b.WriteString(strings.Repeat(" ", 6+i*2) + fmt.Sprintf("k%d:\n", i))
	}

	b.WriteString(strings.Repeat(" ", 6+(maxDepth+4)*2) + "leaf: v\n")

	if _, err := parseAll(map[string]string{"overrides.yaml": b.String()}); err == nil {
		t.Fatal("a document nested past the limit must be rejected")
	}
}

// TestParseIsolatesFailuresToTheKeyThatCausedThem is a regression test.
//
// Parse used to return on the first key that failed, so a typo in one ConfigMap
// key discarded the entries in every other key. The format is one document per
// key precisely so a document can be split by concern or by team, and that is
// only useful if a mistake in one key is contained to it.
func TestParseIsolatesFailuresToTheKeyThatCausedThem(t *testing.T) {
	good := `apiVersion: ` + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: [--flag]
`

	entries, problems, err := Parse(map[string]string{
		"a-broken.yaml": "this is not: [a valid document\n",
		"b-good.yaml":   good,
	})
	if err != nil {
		t.Fatalf("a broken key must not fail the payload as a whole: %v", err)
	}

	if len(entries) != 1 || entries[0].Source.Key != "b-good.yaml" {
		t.Fatalf("entries = %+v, want the entry from the key that parsed", entries)
	}

	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", problems)
	}

	if !problems[0].KeyLevel() || problems[0].Key != "a-broken.yaml" {
		t.Fatalf("problem = %+v, want a key-level problem naming a-broken.yaml", problems[0])
	}
}

// TestParseAttributesEntryFailuresToTheEntry pins that a bad patch names what
// it would have touched, so only those workloads need be withheld.
func TestParseAttributesEntryFailuresToTheEntry(t *testing.T) {
	// An unquoted date resolves to time.Time, which apimachinery cannot carry.
	// The component, kind and sites decode before the patch is normalized, so
	// the failure can still say what it would have changed.
	doc := `apiVersion: ` + APIVersion + `
overrides:
  - component: net
    kind: DaemonSet
    extraArgs:
      node: [--first]
  - component: storage
    kind: DaemonSet
    sites: [edge-west]
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              maintenance: 2026-08-11
`

	entries, problems, err := Parse(map[string]string{"overrides.yaml": doc})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(entries) != 1 || entries[0].Entry.Component != "net" {
		t.Fatalf("entries = %+v, want the net entry to survive", entries)
	}

	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want exactly one", problems)
	}

	problem := problems[0]
	if problem.KeyLevel() {
		t.Fatal("a bad patch is one entry's fault, not the whole key's")
	}

	if problem.Component != "storage" || problem.Kind != "DaemonSet" {
		t.Fatalf("problem targets %s/%s, want storage/DaemonSet", problem.Component, problem.Kind)
	}

	if len(problem.Sites) != 1 || problem.Sites[0] != "edge-west" {
		t.Fatalf("problem sites = %v, want [edge-west]", problem.Sites)
	}

	if problem.Source == nil || problem.Source.Index != 1 {
		t.Fatalf("problem source = %+v, want entry index 1", problem.Source)
	}
}

// TestParsePayloadLimitIsNotAttributable pins that the one limit spanning every
// key is reported as a payload failure rather than blamed on a key.
func TestParsePayloadLimitIsNotAttributable(t *testing.T) {
	big := strings.Repeat("x", maxDocumentBytes)

	_, _, err := Parse(map[string]string{
		"a.yaml": big, "b.yaml": big, "c.yaml": big, "d.yaml": big, "e.yaml": big,
	})
	if err == nil {
		t.Fatal("a payload over the total byte limit must fail as a whole")
	}
}
