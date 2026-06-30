// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unbounded_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
	"github.com/Azure/unbounded/internal/unbounded"
)

// namespaceTemplate names a component's manifest template directory and the
// rendered file (relative to that directory) that declares its Namespace
// resource.
type namespaceTemplate struct {
	component    string
	templatesDir string
	renderedFile string
}

// TestSystemNamespace_MatchesTemplateDefaults is the drift guard between the
// Go-side default (unbounded.SystemNamespace) and the namespace baked into the
// manifest templates via `{{ default "unbounded-system" .Namespace }}`.
//
// It renders each component's templates with NO Namespace supplied (so the
// template `default` fallback is exercised) and asserts the resulting Namespace
// resource name equals unbounded.SystemNamespace. If someone changes the const
// without updating the templates (or vice versa), this test fails loudly.
func TestSystemNamespace_MatchesTemplateDefaults(t *testing.T) {
	root := repoRoot(t)

	templates := []namespaceTemplate{
		{"machina", filepath.Join("deploy", "machina"), "01-namespace.yaml"},
		{"machine-ops", filepath.Join("deploy", "machine-ops"), "00-namespace.yaml"},
		{"orca", filepath.Join("deploy", "orca"), "01-namespace.yaml"},
		{"storage-supervisor", filepath.Join("deploy", "unbounded-storage-supervisor"), "01-namespace.yaml"},
		{"unbounded-operator", filepath.Join("deploy", "unbounded-operator"), "00-namespace.yaml"},
		{"net", filepath.Join("deploy", "net"), "00-namespace.yaml"},
		{"gantry", filepath.Join("deploy", "gantry"), "serviceaccount.yaml"},
		{"inventory", filepath.Join("deploy", "inventory"), filepath.Join("common", "01-namespace.yaml")},
	}

	for _, tc := range templates {
		t.Run(tc.component, func(t *testing.T) {
			outDir := t.TempDir()

			// Render with an empty data map: missing keys evaluate to empty
			// strings, so every `{{ default "unbounded-system" .Namespace }}`
			// falls back to its hardcoded default.
			if err := render.Render(filepath.Join(root, tc.templatesDir), outDir, map[string]string{}); err != nil {
				t.Fatalf("render %s templates: %v", tc.component, err)
			}

			raw, err := os.ReadFile(filepath.Join(outDir, tc.renderedFile))
			if err != nil {
				t.Fatalf("read rendered %s: %v", tc.renderedFile, err)
			}

			name := namespaceResourceName(t, raw)
			if name != unbounded.SystemNamespace {
				t.Fatalf("%s template default namespace %q does not match unbounded.SystemNamespace %q; keep the const and the manifest template defaults in sync",
					tc.component, name, unbounded.SystemNamespace)
			}
		})
	}
}

// namespaceResourceName extracts metadata.name from the `kind: Namespace`
// document in a (possibly multi-document) rendered manifest.
func namespaceResourceName(t *testing.T, manifest []byte) string {
	t.Helper()

	type k8sObject struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}

	for _, doc := range strings.Split(string(manifest), "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" {
			continue
		}

		var obj k8sObject
		if err := yaml.Unmarshal([]byte(doc), &obj); err != nil {
			t.Fatalf("unmarshal rendered document: %v", err)
		}

		if obj.Kind == "Namespace" {
			return obj.Metadata.Name
		}
	}

	t.Fatal("no Namespace resource found in rendered manifest")

	return ""
}

// repoRoot walks up from this test file's directory until it finds go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}

	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("reached filesystem root without finding go.mod (started at %s)", filepath.Dir(file))
		}

		dir = parent
	}
}
