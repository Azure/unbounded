// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundedoperator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

// TestOperatorConfigEndpointAndHashRender asserts the make/GitOps render path
// (`--set APIServerEndpoint=`) both populates the operator-config ConfigMap
// endpoint and stamps a matching config-hash annotation on the Deployment pod
// template. Together these make a re-render with a changed endpoint roll the
// operator (envFrom is read once at pod start), and the shared hash keeps the
// two templates from drifting apart (findings #3/#4).
func TestOperatorConfigEndpointAndHashRender(t *testing.T) {
	t.Parallel()

	const endpoint = "https://api.example.test:6443"

	templatesDir := filepath.Join(repoRoot(t), "deploy", "unbounded-operator")
	outputDir := t.TempDir()

	data := map[string]string{
		"Namespace":         "unbounded-system",
		"OperatorImage":     "operator:test",
		"MetalmanImage":     "metalman:test",
		"APIServerEndpoint": endpoint,
	}
	if err := render.Render(templatesDir, outputDir, data); err != nil {
		t.Fatalf("render.Render: %v", err)
	}

	// The ConfigMap carries the endpoint that the operator reads via envFrom.
	var cm struct {
		Data map[string]string `yaml:"data"`
	}

	readYAML(t, filepath.Join(outputDir, "04-configmap.yaml"), &cm)

	if got := cm.Data["UNBOUNDED_API_SERVER_ENDPOINT"]; got != endpoint {
		t.Fatalf("configmap UNBOUNDED_API_SERVER_ENDPOINT = %q, want %q", got, endpoint)
	}

	// The Deployment pod template carries the matching hash annotation.
	var deploy struct {
		Spec struct {
			Template struct {
				Metadata struct {
					Annotations map[string]string `yaml:"annotations"`
				} `yaml:"metadata"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}

	readYAML(t, filepath.Join(outputDir, "03-deployment.yaml"), &deploy)

	sum := sha256.Sum256([]byte(endpoint))
	want := hex.EncodeToString(sum[:])

	if got := deploy.Spec.Template.Metadata.Annotations["unbounded-cloud.io/operator-config-hash"]; got != want {
		t.Fatalf("deployment operator-config-hash annotation = %q, want %q (sha256 of endpoint)", got, want)
	}
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	if err := yaml.Unmarshal(raw, out); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
}

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
