// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package unboundedoperator

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// staticRender mirrors the inputs of `make unbounded-operator-manifests`.
type staticRender struct {
	namespace string
	image     string
	registry  string
	endpoint  string
	reap      string
}

// renderStatic renders the chart exactly as `make unbounded-operator-manifests`
// does (values-static.yaml plus the make variables, --output-dir) and returns
// the directory holding the files `kubectl unbounded install` embeds.
func renderStatic(t *testing.T, r staticRender) string {
	t.Helper()

	if r.namespace == "" {
		r.namespace = "unbounded-system"
	}

	if r.registry == "" {
		r.registry = "ghcr.io/azure"
	}

	if r.reap == "" {
		r.reap = "true"
	}

	chart := filepath.Join(repoRoot(t), "deploy", "unbounded-operator", "chart")
	out := t.TempDir()

	// Like the Makefile, pass the endpoint with --set-file so commas and other
	// --set syntax survive verbatim.
	endpointFile := filepath.Join(out, "api-server-endpoint")
	if err := os.WriteFile(endpointFile, []byte(r.endpoint), 0o600); err != nil {
		t.Fatal(err)
	}

	runHelm(t,
		"template", "unbounded-operator", chart,
		"--namespace", r.namespace,
		"--values", filepath.Join(chart, "values-static.yaml"),
		"--set-string", "image.reference="+r.image,
		"--set-string", "imageRegistry="+r.registry,
		"--set-file", "apiServerEndpoint="+endpointFile,
		"--set", "reapLegacyResources="+r.reap,
		"--output-dir", filepath.Join(out, "render"),
	)

	return filepath.Join(out, "render", "unbounded-operator", "templates")
}

func runHelm(t *testing.T, args ...string) []byte {
	t.Helper()

	out, err := exec.Command(helmBinary(t), args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return out
}

// TestStaticAndHelmRenders pins the files and objects `kubectl unbounded
// install` and the release manifest rely on, and checks that a Helm or AKS
// cluster extension install renders the same objects minus the Namespace,
// which the release owns.
func TestStaticAndHelmRenders(t *testing.T) {
	t.Parallel()

	layout := map[string][]string{
		"00-namespace.yaml":      {"Namespace/"},
		"01-serviceaccount.yaml": {"ServiceAccount/unbounded-operator"},
		"02-rbac.yaml": {
			"ClusterRole/unbounded-operator",
			"ClusterRoleBinding/unbounded-operator",
			"Role/unbounded-operator-registry-auth-migration",
			"RoleBinding/unbounded-operator-registry-auth-migration",
		},
		"03-configmap.yaml":  {"ConfigMap/unbounded-operator-config"},
		"04-deployment.yaml": {"Deployment/unbounded-operator"},
	}

	for _, tc := range []staticRender{
		{namespace: "unbounded-system", registry: "ghcr.io/azure", reap: "true"},
		{namespace: "flex-system", registry: "registry.example.com/mirror", endpoint: "https://api.example.test:443", reap: "false"},
	} {
		t.Run(tc.namespace, func(t *testing.T) {
			t.Parallel()

			tc.image = tc.registry + "/unbounded-operator:v9.9.9"
			dir := renderStatic(t, tc)

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}

			if len(entries) != len(layout) {
				t.Fatalf("rendered %d files, want %d", len(entries), len(layout))
			}

			static := map[string]map[string]any{}

			for file, keys := range layout {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatalf("read %s: %v", file, err)
				}

				objs := decodeObjects(t, data)

				want := append([]string(nil), keys...)

				if file == "00-namespace.yaml" {
					want = []string{"Namespace/" + tc.namespace}
				}

				sort.Strings(want)

				if got := sortedKeys(objs); !reflect.DeepEqual(got, want) {
					t.Errorf("%s objects = %v, want %v", file, got, want)
				}

				for key, obj := range objs {
					if !strings.HasPrefix(key, "Namespace/") {
						static[key] = obj
					}
				}
			}

			helm := decodeObjects(t, runHelm(t,
				"template", "unbounded-operator", filepath.Join(repoRoot(t), "deploy", "unbounded-operator", "chart"),
				"--namespace", tc.namespace,
				"--set", "image.tag=v9.9.9",
				"--set-string", "imageRegistry="+tc.registry,
				"--set-string", "apiServerEndpoint="+tc.endpoint,
				"--set", "reapLegacyResources="+tc.reap,
			))

			if !reflect.DeepEqual(sortedKeys(helm), sortedKeys(static)) {
				t.Fatalf("helm install objects = %v, static objects = %v", sortedKeys(helm), sortedKeys(static))
			}

			for key, obj := range static {
				if !reflect.DeepEqual(helm[key], obj) {
					gy, _ := yaml.Marshal(helm[key])
					wy, _ := yaml.Marshal(obj)
					t.Errorf("%s differs\n--- helm install\n%s--- static\n%s", key, gy, wy)
				}
			}
		})
	}
}

// TestChartRejectsInvalidValues checks the schema guards that keep the chart
// rendering the same images the operator expects.
func TestChartRejectsInvalidValues(t *testing.T) {
	t.Parallel()

	helm := helmBinary(t)
	chart := filepath.Join(repoRoot(t), "deploy", "unbounded-operator", "chart")

	for name, set := range map[string]string{
		"image name with registry": "image.name=ghcr.io/azure/unbounded-operator",
		"malformed digest":         "image.digest=latest",
		"unknown value":            "operatorImage=foo",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			out, err := exec.Command(helm, "template", "x", chart, "--namespace", "unbounded-system", "--set", set).CombinedOutput()
			if err == nil {
				t.Fatalf("helm template --set %s succeeded, want a schema error\n%s", set, out)
			}
		})
	}
}

// TestChartAcceptsExtensionManagerValues checks that the values AKS cluster
// extensions inject (Azure.Cluster.*) pass the schema.
func TestChartAcceptsExtensionManagerValues(t *testing.T) {
	t.Parallel()

	runHelm(t,
		"template", "unbounded-operator", filepath.Join(repoRoot(t), "deploy", "unbounded-operator", "chart"),
		"--namespace", "unbounded-system",
		"--set-string", "Azure.Cluster.ResourceId=/subscriptions/0000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/c",
		"--set-string", "Azure.Cluster.Region=eastus2euap",
		"--set-string", "Azure.Cluster.Distribution=aks",
	)
}

func helmBinary(t *testing.T) string {
	t.Helper()

	if p := os.Getenv("HELM"); p != "" {
		return p
	}

	if p := filepath.Join(repoRoot(t), "bin", "helm"); fileExists(p) {
		return p
	}

	p, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not found; run `make install-helm` or set HELM")
	}

	return p
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func decodeObjects(t *testing.T, data []byte) map[string]map[string]any {
	t.Helper()

	objects := map[string]map[string]any{}
	dec := yaml.NewDecoder(bytes.NewReader(data))

	for {
		var obj map[string]any

		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			t.Fatalf("decode YAML: %v", err)
		}

		if obj == nil {
			continue
		}

		kind, _ := obj["kind"].(string)
		meta, _ := obj["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		objects[kind+"/"+name] = obj
	}

	return objects
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}
