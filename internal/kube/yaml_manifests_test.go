package kube

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestApplyResourcesV2_SingleResource(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: test-cm
  namespace: default
data:
  key: value
`)

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	err := ApplyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
	require.Equal(t, []string{"test-cm"}, appliedObjects)
}

func TestApplyResourcesV2_MultipleResources(t *testing.T) {
	data := []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-one
  namespace: default
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: cm-two
  namespace: default
`)

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	err := ApplyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
	require.Equal(t, []string{"cm-one", "cm-two"}, appliedObjects)
}

func TestApplyResourcesV2_EmptyDocument(t *testing.T) {
	data := []byte(`---
---
`)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				t.Fatal("Apply should not be called for empty documents")
				return nil
			},
		}).
		Build()

	err := ApplyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.NoError(t, err)
}

func TestApplyResourcesV2_InvalidYAML(t *testing.T) {
	data := []byte(`not: valid: yaml: [`)

	cli := fake.NewClientBuilder().Build()

	err := ApplyManifests(context.Background(), discardLogger(), cli, "test-manager", data)
	require.Error(t, err)
	require.Contains(t, err.Error(), "decoding resource")
}

func TestApplyManifestsV2_WalksDirectory(t *testing.T) {
	dir := t.TempDir()

	// Create nested structure with yaml and non-yaml files.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: deploy-cm
  namespace: default
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "svc.yml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: svc-cm
  namespace: default
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "README.md"), []byte("# ignore me"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("also ignored"), 0o644))

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	err := ApplyManifestsInDirectory(context.Background(), discardLogger(), cli, "test-manager", dir, nil)
	require.NoError(t, err)

	// Both .yaml and .yml files should be applied, but not .md or .txt.
	require.Len(t, appliedObjects, 2)
	require.Contains(t, appliedObjects, "deploy-cm")
	require.Contains(t, appliedObjects, "svc-cm")
}

func TestApplyManifestsV2_NotADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file.yaml")
	require.NoError(t, os.WriteFile(f, []byte("apiVersion: v1"), 0o644))

	cli := fake.NewClientBuilder().Build()

	err := ApplyManifestsInDirectory(context.Background(), discardLogger(), cli, "test-manager", f, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "not a directory")
}

func TestApplyManifestsV2_DirectoryNotFound(t *testing.T) {
	cli := fake.NewClientBuilder().Build()

	err := ApplyManifestsInDirectory(context.Background(), discardLogger(), cli, "test-manager", "/nonexistent/dir", nil)
	require.Error(t, err)
}

func TestApplyManifestsV2_EmptyDirectory(t *testing.T) {
	dir := t.TempDir()

	var applyCalled bool

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, _ runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				applyCalled = true
				return nil
			},
		}).
		Build()

	err := ApplyManifestsInDirectory(context.Background(), discardLogger(), cli, "test-manager", dir, nil)
	require.NoError(t, err)
	require.False(t, applyCalled, "Apply should not be called for an empty directory")
}

func TestApplyManifestsV2_SkipPaths(t *testing.T) {
	dir := t.TempDir()

	// Create nested structure with multiple yaml files.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "deploy.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: deploy-cm
  namespace: default
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "svc.yml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: svc-cm
  namespace: default
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sub", "extra.yaml"), []byte(`apiVersion: v1
kind: ConfigMap
metadata:
  name: extra-cm
  namespace: default
`), 0o644))

	var (
		mu             sync.Mutex
		appliedObjects []string
	)

	cli := fake.NewClientBuilder().
		WithInterceptorFuncs(interceptor.Funcs{
			Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
				u, ok := obj.(interface{ GetName() string })
				if ok {
					mu.Lock()

					appliedObjects = append(appliedObjects, u.GetName())
					mu.Unlock()
				}

				return nil
			},
		}).
		Build()

	skipPaths := []string{
		filepath.Join("sub", "svc.yml"),
	}

	err := ApplyManifestsInDirectory(context.Background(), discardLogger(), cli, "test-manager", dir, skipPaths)
	require.NoError(t, err)

	// deploy.yaml and sub/extra.yaml should be applied, but sub/svc.yml should be skipped.
	require.Len(t, appliedObjects, 2)
	require.Contains(t, appliedObjects, "deploy-cm")
	require.Contains(t, appliedObjects, "extra-cm")
	require.NotContains(t, appliedObjects, "svc-cm")
}
