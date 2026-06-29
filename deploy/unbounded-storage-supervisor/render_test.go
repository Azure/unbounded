// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package unboundedstoragesupervisor

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/Azure/unbounded/hack/cmd/render-manifests/render"
)

func TestRenderStorageSupervisorManifests(t *testing.T) {
	outputDir := t.TempDir()
	data := map[string]string{
		"Namespace": "custom-storage",
		"Image":     "example.azurecr.io/unbounded-storage-supervisor:v1.2.3",
	}

	require.NoError(t, render.Render(".", outputDir, data))

	docs := parseStorageSupervisorRenderedDocs(t, outputDir)
	require.Len(t, docs, 6)

	kinds := map[string]int{}
	for _, doc := range docs {
		require.NotContains(t, doc.raw, "{{")
		require.NotEmpty(t, doc.kind)
		require.NotEmpty(t, doc.name)
		kinds[doc.kind]++
	}

	require.Equal(t, map[string]int{
		"Namespace":          1,
		"ConfigMap":          1,
		"ServiceAccount":     1,
		"ClusterRole":        1,
		"ClusterRoleBinding": 1,
		"DaemonSet":          1,
	}, kinds)

	for _, doc := range docs {
		switch doc.kind {
		case "Namespace":
			require.Equal(t, "custom-storage", doc.name)
		case "ConfigMap", "ServiceAccount", "DaemonSet":
			require.Equal(t, "custom-storage", doc.namespace, "%s/%s should use rendered namespace", doc.kind, doc.name)
		case "ClusterRoleBinding":
			require.Equal(t, "custom-storage", doc.subjectNamespace(t))
		}
	}

	ds := requireStorageSupervisorDoc(t, docs, "DaemonSet", "unbounded-storage-supervisor")
	require.Equal(t, "example.azurecr.io/unbounded-storage-supervisor:v1.2.3", ds.initContainerImage(t, "install"))
	require.Equal(t, "example.azurecr.io/unbounded-storage-supervisor:v1.2.3", ds.containerImage(t, "run"))
}

type storageSupervisorRenderedDoc struct {
	raw       string
	kind      string
	name      string
	namespace string
	doc       map[string]any
}

func parseStorageSupervisorRenderedDocs(t *testing.T, outputDir string) []storageSupervisorRenderedDoc {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(outputDir, "*.yaml"))
	require.NoError(t, err)
	require.NotEmpty(t, files)

	var docs []storageSupervisorRenderedDoc

	for _, file := range files {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)

		dec := yaml.NewDecoder(bytes.NewReader(raw))
		for {
			var doc map[string]any
			err := dec.Decode(&doc)
			if errors.Is(err, io.EOF) {
				break
			}

			require.NoError(t, err)
			if doc == nil {
				continue
			}

			meta, _ := doc["metadata"].(map[string]any)
			name, _ := meta["name"].(string)
			namespace, _ := meta["namespace"].(string)
			kind, _ := doc["kind"].(string)

			docs = append(docs, storageSupervisorRenderedDoc{
				raw:       string(raw),
				kind:      kind,
				name:      name,
				namespace: namespace,
				doc:       doc,
			})
		}
	}

	return docs
}

func requireStorageSupervisorDoc(t *testing.T, docs []storageSupervisorRenderedDoc, kind, name string) storageSupervisorRenderedDoc {
	t.Helper()

	for _, doc := range docs {
		if doc.kind == kind && doc.name == name {
			return doc
		}
	}

	t.Fatalf("missing %s/%s", kind, name)
	return storageSupervisorRenderedDoc{}
}

func (d storageSupervisorRenderedDoc) initContainerImage(t *testing.T, name string) string {
	t.Helper()

	containers := nestedSlice(t, d.doc, "spec", "template", "spec", "initContainers")
	return imageForContainer(t, containers, name)
}

func (d storageSupervisorRenderedDoc) containerImage(t *testing.T, name string) string {
	t.Helper()

	containers := nestedSlice(t, d.doc, "spec", "template", "spec", "containers")
	return imageForContainer(t, containers, name)
}

func (d storageSupervisorRenderedDoc) subjectNamespace(t *testing.T) string {
	t.Helper()

	subjects := nestedSlice(t, d.doc, "subjects")
	require.Len(t, subjects, 1)

	subject, ok := subjects[0].(map[string]any)
	require.True(t, ok)

	namespace, _ := subject["namespace"].(string)
	return namespace
}

func nestedSlice(t *testing.T, doc map[string]any, path ...string) []any {
	t.Helper()

	var cur any = doc
	for _, segment := range path {
		m, ok := cur.(map[string]any)
		require.True(t, ok, "path segment %s should be a map", segment)

		cur = m[segment]
	}

	s, ok := cur.([]any)
	require.True(t, ok, "%s should be a slice", strings.Join(path, "."))

	return s
}

func imageForContainer(t *testing.T, containers []any, name string) string {
	t.Helper()

	for _, item := range containers {
		container, ok := item.(map[string]any)
		require.True(t, ok)

		if container["name"] == name {
			image, _ := container["image"].(string)
			return image
		}
	}

	t.Fatalf("missing container %s", name)
	return ""
}
