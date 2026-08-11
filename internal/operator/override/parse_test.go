// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package override

import (
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
	entries, err := Parse(map[string]string{"overrides.yaml": validDocument})
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
		entries, err := Parse(data)
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
	entries, err := Parse(map[string]string{
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
			_, err := Parse(map[string]string{"overrides.yaml": tc.doc})
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
	_, err := Parse(map[string]string{
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
	entries, err := Parse(map[string]string{"overrides.yaml": `apiVersion: ` + APIVersion + `
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
