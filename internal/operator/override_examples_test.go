// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package operator

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	machinamanifests "github.com/Azure/unbounded/deploy/machina"
	netmanifests "github.com/Azure/unbounded/deploy/net"
	storagemanifests "github.com/Azure/unbounded/deploy/unbounded-storage-supervisor"
	"github.com/Azure/unbounded/internal/metalman/commands"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/operator/components/metalman"
	"github.com/Azure/unbounded/internal/operator/override"
)

// componentManifests maps a component name to the manifest set it renders from.
//
// metalman is deliberately absent. Its Deployment is built in Go rather than
// decoded from a manifest, and pointing it at the machina manifest set (which
// is where its RBAC ships) resolved its container names against the machina
// controller instead: the map claimed metalman's container was
// machina-controller. See componentContainerNames.
var componentManifests = map[string]fs.FS{
	"net":     netmanifests.Manifests,
	"machina": machinamanifests.Manifests,
	"gantry":  gantrymanifests.Manifests,
	"storage": storagemanifests.Manifests,
}

// componentContainerNames returns the container names an example may refer to
// for a component, taken from what that component actually plans.
//
// Most components decode their workloads from embedded manifests, so reading
// the manifests is reading the truth. metalman constructs its Deployment in Go,
// so the only honest source is the component itself.
func componentContainerNames(t *testing.T, componentName, kind string) map[string]bool {
	t.Helper()

	if componentName == "metalman" {
		return metalmanContainerNames(t)
	}

	manifests, known := componentManifests[componentName]
	if !known {
		t.Fatalf("example targets unknown component %q", componentName)
	}

	return manifestContainerNames(t, manifests, kind)
}

// metalmanContainerNames plans the metalman component and reads the container
// names off the Deployment it produces. Planning needs no client: it decodes
// the shared RBAC from the machina manifests and builds the Deployment from the
// Site.
func metalmanContainerNames(t *testing.T) map[string]bool {
	t.Helper()

	enabled := true
	site := &unboundedv1alpha3.Site{
		ObjectMeta: metav1.ObjectMeta{Name: "example", UID: "example-uid"},
		Spec: unboundedv1alpha3.SiteSpec{
			Components: unboundedv1alpha3.SiteComponents{
				Metalman: &unboundedv1alpha3.MetalmanComponentSpec{
					SiteComponentSpec: unboundedv1alpha3.SiteComponentSpec{Enabled: &enabled},
				},
			},
		},
	}

	env := &component.Env{Namespace: component.DefaultNamespace}

	plan, _, err := metalman.New().Plan(t.Context(), env, site)
	if err != nil {
		t.Fatalf("plan metalman: %v", err)
	}

	names := map[string]bool{}

	for _, op := range plan.Operations {
		if op.Object.GetKind() != "Deployment" {
			continue
		}

		for _, field := range []string{"containers", "initContainers"} {
			containers, _, err := unstructured.NestedSlice(op.Object.Object, "spec", "template", "spec", field)
			if err != nil {
				t.Fatalf("read metalman %s: %v", field, err)
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

	if len(names) == 0 {
		t.Fatal("metalman planned no containers; the resolver is broken")
	}

	return names
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
			entries, problems, err := override.Parse(map[string]string{example.source: example.document})
			if err != nil {
				t.Fatalf("example does not parse: %v", err)
			}

			if err := override.ProblemsError(problems); err != nil {
				t.Fatalf("example does not parse: %v", err)
			}

			if err := override.ValidateErr(entries); err != nil {
				t.Fatalf("example does not validate: %v", err)
			}

			for _, entry := range entries {
				assertExampleContainersExist(t, entry)
				assertExampleExtraArgsResolve(t, example, entry)
			}
		})
	}
}

// assertExampleContainersExist checks every container an example names against
// the component's rendered manifests, ignoring containers it declares as
// additions.
func assertExampleContainersExist(t *testing.T, entry override.SourcedEntry) {
	t.Helper()

	existing := componentContainerNames(t, entry.Entry.Component, entry.Entry.Kind)

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

// componentFlagSets maps a component to the command whose flags its container
// actually parses, for the components this package can reach.
//
// Only metalman qualifies today: its command is built in internal/, so this
// package may import it. machina, net, gantry and storage all define their
// flags in cmd/ packages, which internal/ must not import (see AGENTS.md), so
// an extraArgs example naming one of those cannot be checked here.
var componentFlagSets = map[string]func() *cobra.Command{
	"metalman": commands.ServePXECmd,
}

// unverifiableExampleFlags records the extraArgs flags in shipped examples that
// componentFlagSets cannot check, so each is a deliberate, reviewed entry
// rather than an accident.
//
// It is deliberately empty. Both user-facing surfaces, the reference doc and
// the example ConfigMap, use a component this package can verify. Adding an
// entry here means someone has read the component's flag registration by hand
// and is asserting the flag exists; the review of that assertion is the point.
//
// The rule exists because the alternative is what shipped: the documentation
// and the example both told users to append --max-concurrent-reconciles to
// machina-controller, which registers exactly one flag (--config) and exits
// non-zero on anything else. A user following the documented "safe way to add
// arguments" would have crash-looped the machina controller.
var unverifiableExampleFlags = map[string]bool{}

// assertExampleExtraArgsResolve checks that every flag an example appends is a
// flag the component actually accepts.
//
// Container names being right is not enough. These components are cobra and
// clap programs, and every one of them exits non-zero on an unrecognised flag,
// so a wrong flag in a documented example is a CrashLoopBackOff for anyone who
// copies it. The operator cannot check this at runtime, which is exactly why
// the examples have to be checked here.
func assertExampleExtraArgsResolve(t *testing.T, example overrideExample, entry override.SourcedEntry) {
	t.Helper()

	if len(entry.Entry.ExtraArgs) == 0 {
		return
	}

	build, verifiable := componentFlagSets[entry.Entry.Component]

	containers := make([]string, 0, len(entry.Entry.ExtraArgs))
	for name := range entry.Entry.ExtraArgs {
		containers = append(containers, name)
	}

	sort.Strings(containers)

	for _, container := range containers {
		for _, arg := range entry.Entry.ExtraArgs[container] {
			name := flagName(arg)
			if name == "" {
				continue
			}

			if !verifiable {
				if unverifiableExampleFlags[entry.Entry.Component+" "+name] {
					continue
				}

				t.Errorf("%s appends %q to component %q, whose flags this package cannot reach "+
					"(its command is defined under cmd/). Either use a component in componentFlagSets, "+
					"or record %q in unverifiableExampleFlags after checking the flag by hand.",
					example.source, arg, entry.Entry.Component, entry.Entry.Component+" "+name)

				continue
			}

			if build().Flags().Lookup(name) == nil {
				t.Errorf("%s appends %q, but %s registers no --%s flag; "+
					"an unrecognised flag makes the component exit non-zero",
					example.source, arg, entry.Entry.Component, name)
			}
		}
	}
}

// flagName extracts the long flag name from an argument, or an empty string
// when the argument is not a long flag and so cannot be checked this way.
func flagName(arg string) string {
	if !strings.HasPrefix(arg, "--") {
		return ""
	}

	name := strings.TrimPrefix(arg, "--")
	if equals := strings.Index(name, "="); equals >= 0 {
		name = name[:equals]
	}

	return name
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

// TestDocumentedPathsMatchTheAllowlist holds the reference documentation
// against the allowlist the operator compiles.
//
// The doc tells users that anything not listed is rejected, which is only
// useful if the list is right. PermittedPaths and ProtectedPaths are exported
// for exactly this, and nothing consumed them, so the doc listed nothing at all
// and a user's only way to learn the surface was to trip over errors.
//
// The comparison runs in both directions: a path added to the allowlist and not
// to the doc is undiscoverable, and a path in the doc and not the allowlist is
// a promise the operator does not keep.
func TestDocumentedPathsMatchTheAllowlist(t *testing.T) {
	doc := filepath.Join(repoRootForExamples(t), "docs", "content", "reference", "workload-overrides.md")

	contents, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}

	for _, section := range []struct {
		name     string
		declared []string
	}{
		{name: "permitted paths", declared: override.PermittedPaths()},
		{name: "protected paths", declared: override.ProtectedPaths()},
	} {
		t.Run(section.name, func(t *testing.T) {
			documented := documentedPaths(t, string(contents), section.name)

			if !slices.Equal(documented, section.declared) {
				t.Errorf("the %s section of workload-overrides.md is out of date.\n documented: %v\n allowlist:  %v\n\n"+
					"Regenerate the list between the BEGIN and END markers from override.PermittedPaths and "+
					"override.ProtectedPaths.", section.name, documented, section.declared)
			}
		})
	}
}

// documentedPaths extracts the bulleted paths from a generated block, so the
// prose around them can change freely without breaking the comparison.
func documentedPaths(t *testing.T, contents, section string) []string {
	t.Helper()

	var (
		begin = "<!-- BEGIN GENERATED: " + section + " -->"
		end   = "<!-- END GENERATED: " + section + " -->"
	)

	from := strings.Index(contents, begin)
	to := strings.Index(contents, end)

	if from < 0 || to < 0 || to < from {
		t.Fatalf("workload-overrides.md has no %q block; the generated markers must not be removed", section)
	}

	var paths []string

	for _, line := range strings.Split(contents[from+len(begin):to], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- `") || !strings.HasSuffix(line, "`") {
			continue
		}

		paths = append(paths, strings.TrimSuffix(strings.TrimPrefix(line, "- `"), "`"))
	}

	return paths
}

// TestDocumentedComponentKindsMatchTheTable holds the component/kind table in
// the reference documentation against what validation actually accepts. Seven
// of the ten pairs the two lists once accepted between them could never match
// anything, which is the mistake this table exists to prevent users repeating.
func TestDocumentedComponentKindsMatchTheTable(t *testing.T) {
	doc := filepath.Join(repoRootForExamples(t), "docs", "content", "reference", "workload-overrides.md")

	contents, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}

	for _, component := range []string{"net", "machina", "gantry", "metalman", "storage"} {
		kinds := override.ComponentKinds(component)
		if len(kinds) == 0 {
			t.Fatalf("override.ComponentKinds(%q) is empty; the test is looking at the wrong names", component)
		}

		row := "| `" + component + "` | "

		line, found := findLine(string(contents), row)
		if !found {
			t.Errorf("workload-overrides.md has no component table row for %q", component)

			continue
		}

		for _, kind := range kinds {
			if !strings.Contains(line, "`"+kind+"`") {
				t.Errorf("the component table says %q emits %s, but validation accepts %v",
					component, line, kinds)
			}
		}
	}
}

func findLine(contents, prefix string) (string, bool) {
	for _, line := range strings.Split(contents, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}

	return "", false
}
