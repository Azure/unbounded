// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifests

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// TestProductionManifestsRender renders every *.yaml.tmpl under
// deploy/orca/ (excluding the dev/ subdirectory which contains the
// in-Kind LocalStack/Azurite manifests) with realistic inputs and
// asserts the output is structurally valid Kubernetes YAML.
func TestProductionManifestsRender(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	templatesDir := filepath.Join(root, "deploy", "orca")

	renderAndValidate(t, templatesDir, productionData(),
		// One file at a time: walking the dev/ subdirectory is the dev
		// suite's job, so we render-then-skip it here.
		skipDir("dev"),
		// Required kinds that MUST appear at least once across the
		// rendered manifests.
		expectKindsAtLeastOnce("Namespace", "Deployment", "Service", "ConfigMap"),
	)
}

// TestDeploymentAntiAffinityModes asserts the strict vs. relaxed
// anti-affinity render paths controlled by the RequireAntiAffinity
// template knob. Kind installs render `required...`; non-kind
// installs render `preferred...`. This keeps small dev clusters
// schedulable while preserving the strict topology on kind.
func TestDeploymentAntiAffinityModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		setValue  string
		wantField string
		denyField string
	}{
		{
			name:      "required",
			setValue:  "true",
			wantField: "requiredDuringSchedulingIgnoredDuringExecution",
			denyField: "preferredDuringSchedulingIgnoredDuringExecution",
		},
		{
			name:      "preferred",
			setValue:  "false",
			wantField: "preferredDuringSchedulingIgnoredDuringExecution",
			denyField: "requiredDuringSchedulingIgnoredDuringExecution",
		},
	}

	root := repoRoot(t)
	templatesDir := filepath.Join(root, "deploy", "orca")

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			data := productionData()
			data["RequireAntiAffinity"] = tt.setValue

			outputDir := t.TempDir()
			if err := render.Render(templatesDir, outputDir, data); err != nil {
				t.Fatalf("render.Render: %v", err)
			}

			raw, err := os.ReadFile(filepath.Join(outputDir, "04-deployment.yaml"))
			if err != nil {
				t.Fatalf("read deployment: %v", err)
			}

			body := string(raw)
			if !strings.Contains(body, tt.wantField) {
				t.Errorf("RequireAntiAffinity=%q: expected %q in rendered deployment\n---\n%s",
					tt.setValue, tt.wantField, body)
			}

			if strings.Contains(body, tt.denyField) {
				t.Errorf("RequireAntiAffinity=%q: did not expect %q in rendered deployment\n---\n%s",
					tt.setValue, tt.denyField, body)
			}
		})
	}
}

// TestDevManifestsRender renders the LocalStack + Azurite manifests
// used by the Kind dev harness. The previous one-shot bucket-init
// Jobs (02-init-job.yaml.tmpl, 04-azurite-init.yaml.tmpl) were
// replaced by self-healing PostStart lifecycle hooks / sidecars
// driven by an inline ConfigMap, so the required-kind set is
// Deployment + Service + ConfigMap rather than the old
// Deployment + Service + Job.
func TestDevManifestsRender(t *testing.T) {
	t.Parallel()

	root := repoRoot(t)
	templatesDir := filepath.Join(root, "deploy", "orca", "dev")

	renderAndValidate(t, templatesDir, devData(),
		expectKindsAtLeastOnce("Deployment", "Service", "ConfigMap"),
		// Dev emulator Services must be ClusterIP with no nodePort
		// fields. orcadev port-forwards everywhere, so fixed
		// NodePorts are an AKS / shared-cluster footgun we
		// deliberately removed.
		expectAllServicesClusterIP(),
	)
}

// productionData supplies realistic template variables for the
// production-shape templates. Templates use sprig's `default` for
// missing keys; we set values that exercise the non-default paths
// where it matters.
func productionData() map[string]string {
	return map[string]string{
		"Namespace":               "orca-test",
		"Image":                   "ghcr.io/example/orca:test",
		"ImagePullPolicy":         "IfNotPresent",
		"TargetReplicas":          "3",
		"OriginID":                "test-origin",
		"OriginDriver":            "awss3",
		"OriginAWSS3Endpoint":     "http://localstack:4566",
		"OriginAWSS3Region":       "us-east-1",
		"OriginAWSS3Bucket":       "orca-origin",
		"OriginAWSS3UsePathStyle": "true",
		"CachestoreEndpoint":      "http://localstack:4566",
		"CachestoreBucket":        "orca-cache",
		"CachestoreRegion":        "us-east-1",
		"ClusterService":          "orca-peers.orca-test.svc.cluster.local",
		"ServerAuthEnabled":       "false",
		"InternalTLSEnabled":      "false",
		"AzureAccount":            "",
		"AzureContainer":          "",
		"AzureEndpoint":           "",
	}
}

func devData() map[string]string {
	return map[string]string{
		"Namespace":        "orca-test",
		"CachestoreBucket": "orca-cache",
		"OriginBucket":     "orca-origin",
		"AzuriteContainer": "orca-test",
	}
}

// renderAndValidate renders every template under templatesDir into a
// t.TempDir, then walks the output and applies each Validator.
func renderAndValidate(t *testing.T, templatesDir string, data map[string]string, validators ...Validator) {
	t.Helper()

	outputDir := t.TempDir()

	if err := render.Render(templatesDir, outputDir, data); err != nil {
		t.Fatalf("render.Render: %v", err)
	}
	// Collect every rendered .yaml file. Skip directories filtered
	// by the validators.
	skipDirs := skipDirsOf(validators)

	var renderedFiles []string

	walkErr := filepath.WalkDir(outputDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			rel, _ := filepath.Rel(outputDir, path)
			if _, skip := skipDirs[rel]; skip {
				return filepath.SkipDir
			}

			return nil
		}

		if strings.HasSuffix(path, ".yaml") {
			renderedFiles = append(renderedFiles, path)
		}

		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk rendered output: %v", walkErr)
	}

	if len(renderedFiles) == 0 {
		t.Fatalf("no rendered manifests found under %s", outputDir)
	}

	sort.Strings(renderedFiles)

	docs := parseRenderedDocs(t, renderedFiles)

	// Always-on basic structural validation.
	for _, d := range docs {
		validateBasicStructure(t, d)
	}

	for _, v := range validators {
		v.Validate(t, docs)
	}
}

// renderedDoc is one logical YAML document plus the source file it
// came from (multi-doc files split into multiple renderedDocs).
type renderedDoc struct {
	SourcePath string
	Index      int
	Doc        map[string]any
}

func parseRenderedDocs(t *testing.T, files []string) []renderedDoc {
	t.Helper()

	var docs []renderedDoc

	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}

		dec := yaml.NewDecoder(bytes.NewReader(raw))

		for i := 0; ; i++ {
			var doc map[string]any
			if derr := dec.Decode(&doc); derr != nil {
				if errors.Is(derr, io.EOF) {
					break
				}

				t.Fatalf("yaml decode %s doc %d: %v", f, i, derr)
			}

			if doc == nil {
				continue
			}

			docs = append(docs, renderedDoc{SourcePath: f, Index: i, Doc: doc})
		}
	}

	return docs
}

func validateBasicStructure(t *testing.T, d renderedDoc) {
	t.Helper()

	apiVersion, _ := d.Doc["apiVersion"].(string)
	kind, _ := d.Doc["kind"].(string)

	if apiVersion == "" {
		t.Errorf("%s doc %d: missing apiVersion", d.SourcePath, d.Index)
	}

	if kind == "" {
		t.Errorf("%s doc %d: missing kind", d.SourcePath, d.Index)
	}

	meta, _ := d.Doc["metadata"].(map[string]any)
	if meta == nil {
		t.Errorf("%s doc %d (kind=%s): missing metadata", d.SourcePath, d.Index, kind)
		return
	}

	name, _ := meta["name"].(string)
	if name == "" {
		t.Errorf("%s doc %d (kind=%s): missing metadata.name", d.SourcePath, d.Index, kind)
	}
}

// Validator is a test-time check applied to the full set of
// rendered docs.
type Validator interface {
	Validate(t *testing.T, docs []renderedDoc)
	skipDir() string // empty when not a dir filter
}

type kindsAtLeastOnce struct{ kinds []string }

func (v kindsAtLeastOnce) Validate(t *testing.T, docs []renderedDoc) {
	t.Helper()

	seen := map[string]bool{}

	for _, d := range docs {
		if k, _ := d.Doc["kind"].(string); k != "" {
			seen[k] = true
		}
	}

	for _, want := range v.kinds {
		if !seen[want] {
			t.Errorf("expected at least one document of kind %q, got kinds %v", want, sortedKeys(seen))
		}
	}
}

func (v kindsAtLeastOnce) skipDir() string { return "" }

func expectKindsAtLeastOnce(kinds ...string) Validator {
	return kindsAtLeastOnce{kinds: kinds}
}

// allServicesClusterIP asserts every rendered Service is ClusterIP
// (or has an empty type, which is also ClusterIP per Kubernetes
// defaulting) and carries no nodePort fields. Dev orcadev relies on
// `kubectl port-forward`, not NodePort, so a NodePort rendering would
// silently regress the AKS / shared-cluster install path.
type allServicesClusterIP struct{}

func (allServicesClusterIP) Validate(t *testing.T, docs []renderedDoc) {
	t.Helper()

	for _, d := range docs {
		if k, _ := d.Doc["kind"].(string); k != "Service" {
			continue
		}

		spec, _ := d.Doc["spec"].(map[string]any)
		if spec == nil {
			t.Errorf("%s doc %d: Service has no spec", d.SourcePath, d.Index)
			continue
		}

		if svcType, _ := spec["type"].(string); svcType != "" && svcType != "ClusterIP" {
			t.Errorf("%s doc %d: Service type %q; want ClusterIP", d.SourcePath, d.Index, svcType)
		}

		ports, _ := spec["ports"].([]any)
		for i, p := range ports {
			pm, _ := p.(map[string]any)
			if pm == nil {
				continue
			}

			if _, hasNodePort := pm["nodePort"]; hasNodePort {
				t.Errorf("%s doc %d: Service ports[%d] has nodePort; expected ClusterIP-only", d.SourcePath, d.Index, i)
			}
		}
	}
}

func (allServicesClusterIP) skipDir() string { return "" }

func expectAllServicesClusterIP() Validator { return allServicesClusterIP{} }

type dirSkipper struct{ name string }

func (d dirSkipper) Validate(*testing.T, []renderedDoc) {}

func (d dirSkipper) skipDir() string { return d.name }

func skipDir(name string) Validator {
	return dirSkipper{name: name}
}

func skipDirsOf(vs []Validator) map[string]struct{} {
	out := map[string]struct{}{}

	for _, v := range vs {
		if d := v.skipDir(); d != "" {
			out[d] = struct{}{}
		}
	}

	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// repoRoot returns the absolute path to the repo root by walking up
// from this test file's directory until it finds a go.mod.
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
