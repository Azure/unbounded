package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ApplyManifestsInDirectory walks manifestDir recursively and applies every .yaml/.yml
// file it finds using server-side apply via the controller-runtime client.
// Any file whose path relative to manifestDir appears in skipPaths is skipped.
func ApplyManifestsInDirectory(ctx context.Context, logger *slog.Logger, k8sClient client.Client, fieldManager, manifestDir string, skipPaths []string) error {
	info, err := os.Stat(manifestDir)
	if err != nil {
		return fmt.Errorf("stat manifest directory: %w", err)
	}

	if !info.IsDir() {
		return fmt.Errorf("manifest path is not a directory: %s", manifestDir)
	}

	skip := make(map[string]struct{}, len(skipPaths))
	for _, p := range skipPaths {
		skip[filepath.Clean(p)] = struct{}{}
	}

	return filepath.WalkDir(manifestDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}

		rel, err := filepath.Rel(manifestDir, path)
		if err != nil {
			return fmt.Errorf("computing relative path for %s: %w", path, err)
		}

		if _, ok := skip[filepath.Clean(rel)]; ok {
			logger.Info("skipping manifest", "file", path)
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		logger.Info("applying manifest", "file", path)

		if err := ApplyManifests(ctx, logger, k8sClient, fieldManager, data); err != nil {
			return fmt.Errorf("applying %s: %w", path, err)
		}

		return nil
	})
}

// ApplyManifests decodes one or more YAML/JSON resources from data and
// applies each one to the cluster using server-side apply.
func ApplyManifests(ctx context.Context, logger *slog.Logger, k8sClient client.Client, fieldManager string, data []byte) error {
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)

	for {
		obj := &unstructured.Unstructured{}
		if err := decoder.Decode(obj); err != nil {
			if err == io.EOF {
				break
			}

			return fmt.Errorf("decoding resource: %w", err)
		}

		if obj.Object == nil {
			continue
		}

		applyCfg := client.ApplyConfigurationFromUnstructured(obj)
		if err := k8sClient.Apply(ctx, applyCfg, client.FieldOwner(fieldManager), client.ForceOwnership); err != nil {
			return fmt.Errorf("applying %s %q: %w", obj.GetKind(), obj.GetName(), err)
		}

		logger.Info("resource applied", "kind", obj.GetKind(), "name", obj.GetName())
	}

	return nil
}
