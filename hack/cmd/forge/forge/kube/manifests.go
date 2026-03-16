package kube

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"text/template"

	"github.com/Masterminds/sprig/v3"
)

func ApplyManifests(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, manifestPath string) error {
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return fmt.Errorf("manifest not found: %w", err)
	}

	l := logger.With("kubectl_op", "apply", "manifest", manifestPath)

	return KubectlCmd(ctx, l, kubectl, "apply", "-f", manifestPath)
}

func ApplyManifestsInDirectory(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, manifestPath string) error {
	if _, err := os.Stat(manifestPath); os.IsNotExist(err) {
		return fmt.Errorf("manifest not found: %w", err)
	}

	l := logger.With("kubectl_op", "apply", "manifest", manifestPath)

	return KubectlCmd(ctx, l, kubectl, "apply", "-R", "-f", manifestPath)
}

// RenderManifestsInDirectory renders all Go text templates from sourceDir in the given fs
// into destDir, using the provided templateData for template execution.
func RenderManifestsInDirectory(fileSystem fs.FS, sourceDir, destDir string, templateData any) error {
	return fs.WalkDir(fileSystem, sourceDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Get relative path from sourceDir
		relPath, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return fmt.Errorf("get relative path: %w", err)
		}

		// Skip the root directory
		if relPath == "." {
			return nil
		}

		targetPath := filepath.Join(destDir, relPath)

		// Remove .tmpl extension if present
		if filepath.Ext(targetPath) == ".tmpl" {
			targetPath = targetPath[:len(targetPath)-len(".tmpl")]
		}

		if d.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}

		// Read the file content
		data, err := fs.ReadFile(fileSystem, path)
		if err != nil {
			return fmt.Errorf("read file %s: %w", path, err)
		}

		// Parse and execute the template
		tmpl, err := template.New(filepath.Base(path)).Funcs(sprig.FuncMap()).Parse(string(data))
		if err != nil {
			return fmt.Errorf("parse template %s: %w", path, err)
		}

		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, templateData); err != nil {
			return fmt.Errorf("execute template %s: %w", path, err)
		}

		// Ensure parent directory exists
		if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
			return fmt.Errorf("create directory %s: %w", filepath.Dir(targetPath), err)
		}

		// Write the rendered content
		if err := os.WriteFile(targetPath, buf.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write file %s: %w", targetPath, err)
		}

		return nil
	})
}

func WriteAndApplyManifest(ctx context.Context, logger *slog.Logger, kubectl func(context.Context) *exec.Cmd, manifestFile string, manifest []byte) error {
	if err := os.WriteFile(manifestFile, manifest, 0o644); err != nil {
		return fmt.Errorf("error writing manifest: %w", err)
	}

	if err := ApplyManifests(ctx, logger, kubectl, manifestFile); err != nil {
		return fmt.Errorf("apply manifest: %w", err)
	}

	return nil
}
