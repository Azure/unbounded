// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/override"
)

// componentManifests maps a component name to the manifest set it renders from.
var componentManifests = map[string]fs.FS{
	"net":      netmanifests.Manifests,
	"machina":  machinamanifests.Manifests,
	"gantry":   gantrymanifests.Manifests,
	"metalman": machinamanifests.Manifests,
	"storage":  storagemanifests.Manifests,
}

// TestDocumentedOverrideExamplesResolve validates every override example the
// repository ships and checks that the containers they name actually exist.
//
// This test earns its place. The design document warns that container names are
// release-specific, and then violated that rule in its own examples for two
// revisions: the storage example named a container "supervisor" when the
// containers are "install" and "run", and the machina example named
// "controller" when it is "machina-controller". A document that cannot keep its
// own examples resolvable is evidence that users will not either.
//
// It relies on `make test` rendering the manifests first.
func TestDocumentedOverrideExamplesResolve(t *testing.T) {
	examples := collectOverrideExamples(t)
	if len(examples) == 0 {
		t.Fatal("found no override examples; the extractor is probably broken")
	}

	for _, example := range examples {
		t.Run(example.source, func(t *testing.T) {
			entries, err := override.Parse(map[string]string{example.source: example.document})
			if err != nil {
				t.Fatalf("example does not parse: %v", err)
			}

			if err := override.Validate(entries); err != nil {
				t.Fatalf("example does not validate: %v", err)
			}

			for _, entry := range entries {
				assertExampleContainersExist(t, entry)
			}
		})
	}
}

// assertExampleContainersExist checks every container an example names against
// the component's rendered manifests, ignoring containers it declares as
// additions.
func assertExampleContainersExist(t *testing.T, entry override.SourcedEntry) {
	t.Helper()

	manifests, known := componentManifests[entry.Entry.Component]
	if !known {
		t.Fatalf("example targets unknown component %q", entry.Entry.Component)
	}

	existing := manifestContainerNames(t, manifests, entry.Entry.Kind)

	added := map[string]bool{}
	for _, name := range append(append([]string{}, entry.Entry.AddContainers...), entry.Entry.AddInitContainers...) {
		added[name] = true
	}

	for name := range entry.Entry.ExtraArgs {
		if !existing[name] && !added[name] {
			t.Errorf("extraArgs names container %q, which no %s %s in the %s manifests has (have: %s)",
				name, entry.Entry.Component, entry.Entry.Kind, entry.Entry.Component, sortedKeys(existing))
		}
	}

	for _, field := range []string{"containers", "initContainers"} {
		for _, name := range patchContainerNames(entry.Entry.Patch, field) {
			if !existing[name] && !added[name] {
				t.Errorf("patch names %s %q, which no %s %s in the %s manifests has (have: %s)",
					field, name, entry.Entry.Component, entry.Entry.Kind, entry.Entry.Component, sortedKeys(existing))
			}
		}
	}
}

// manifestContainerNames collects the container names of every workload of a
// kind in a manifest set. Per-Site components rename their workloads at plan
// time but keep their container names, so matching on kind alone is enough.
func manifestContainerNames(t *testing.T, manifests fs.FS, kind string) map[string]bool {
	t.Helper()

	names := map[string]bool{}

	files, err := component.YamlFiles(manifests)
	if err != nil {
		t.Fatalf("list manifests: %v", err)
	}

	for _, file := range files {
		data, err := fs.ReadFile(manifests, file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}

		for _, doc := range strings.Split(string(data), "\n---") {
			doc = strings.TrimSpace(doc)
			if doc == "" {
				continue
			}

			var obj unstructured.Unstructured
			if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
				continue
			}

			if obj.GetKind() != kind {
				continue
			}

			for _, field := range []string{"containers", "initContainers"} {
				containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", field)
				if err != nil {
					continue
				}

				for _, raw := range containers {
					container, ok := raw.(map[string]any)
					if !ok {
						continue
					}

					if name, ok := container["name"].(string); ok && name != "" {
						names[name] = true
					}
				}
			}
		}
	}

	return names
}

// overrideExample is one example document and where it came from.
type overrideExample struct {
	source   string
	document string
}

// collectOverrideExamples gathers every override document the repository ships:
// the example ConfigMap, and any fenced YAML block in the docs or design that
// declares the overrides apiVersion.
func collectOverrideExamples(t *testing.T) []overrideExample {
	t.Helper()

	root := repoRootForExamples(t)

	var examples []overrideExample

	examples = append(examples, exampleConfigMapDocuments(t, root)...)

	for _, path := range []string{
		filepath.Join(root, "docs", "content", "reference", "workload-overrides.md"),
		filepath.Join(root, "designs", "component-workload-overrides.md"),
	} {
		examples = append(examples, fencedOverrideDocuments(t, path)...)
	}

	return examples
}

// exampleConfigMapDocuments reads each data key of the shipped example
// ConfigMap.
func exampleConfigMapDocuments(t *testing.T, root string) []overrideExample {
	t.Helper()

	path := filepath.Join(root, "deploy", "unbounded-operator", "examples", "component-overrides.example.yaml")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example ConfigMap: %v", err)
	}

	var configMap struct {
		Data map[string]string `json:"data"`
	}

	if err := yaml.Unmarshal(data, &configMap); err != nil {
		t.Fatalf("parse example ConfigMap: %v", err)
	}

	examples := make([]overrideExample, 0, len(configMap.Data))
	for key, document := range configMap.Data {
		examples = append(examples, overrideExample{source: "example ConfigMap/" + key, document: document})
	}

	return examples
}

// fencedOverrideDocuments extracts fenced YAML blocks that are override
// documents, so prose examples cannot drift from reality either.
func fencedOverrideDocuments(t *testing.T, path string) []overrideExample {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var (
		examples []overrideExample
		current  []string
		inFence  bool
		index    int
	)

	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				if document, ok := overrideDocumentFrom(strings.Join(current, "\n")); ok {
					examples = append(examples, overrideExample{
						source:   filepath.Base(path) + "#" + strconv.Itoa(index),
						document: document,
					})
					index++
				}

				current = nil
			}

			inFence = !inFence

			continue
		}

		if inFence {
			current = append(current, line)
		}
	}

	return examples
}

// overrideDocumentFrom recognizes both whole documents and the indented entry
// fragments the documentation uses, wrapping the latter so they can be parsed.
//
// Fragments matter most: they are the snippets a reader copies, and the ones
// that silently drift when a container is renamed.
func overrideDocumentFrom(block string) (string, bool) {
	trimmed := strings.TrimSpace(block)

	if strings.Contains(block, "apiVersion: "+override.APIVersion) {
		// A full ConfigMap example embeds its documents in data keys, which are
		// covered separately by the shipped example file.
		if strings.Contains(block, "kind: ConfigMap") {
			return "", false
		}

		return block, true
	}

	if !strings.HasPrefix(trimmed, "- component:") {
		return "", false
	}

	// Re-indent the fragment to a consistent two spaces so it parses as a list
	// under `overrides:`.
	indent := len(block) - len(strings.TrimLeft(block, " \n"))

	var lines []string

	for _, line := range strings.Split(block, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}

		if len(line) >= indent {
			line = line[indent:]
		}

		lines = append(lines, "  "+line)
	}

	return "apiVersion: " + override.APIVersion + "\noverrides:\n" + strings.Join(lines, "\n") + "\n", true
}

func repoRootForExamples(t *testing.T) string {
	t.Helper()

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}

	return filepath.Join(filepath.Dir(thisFile), "..", "..")
}

func patchContainerNames(patch map[string]any, field string) []string {
	containers, found, err := unstructured.NestedSlice(patch, "spec", "template", "spec", field)
	if err != nil || !found {
		return nil
	}

	var names []string

	for _, raw := range containers {
		container, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		if name, ok := container["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}

	return names
}

func sortedKeys(set map[string]bool) string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	return strings.Join(keys, ", ")
}
