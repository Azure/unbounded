package kube

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestRenderManifestsInDirectory(t *testing.T) {
	tests := []struct {
		name         string
		fsys         fstest.MapFS
		sourceDir    string
		templateData any
		wantFiles    map[string]string
		wantErr      bool
	}{
		{
			name: "renders single template file",
			fsys: fstest.MapFS{
				"source/manifest.yaml": &fstest.MapFile{
					Data: []byte("name: {{ .Name }}\nversion: {{ .Version }}"),
				},
			},
			sourceDir: "source",
			templateData: struct {
				Name    string
				Version string
			}{
				Name:    "my-app",
				Version: "1.0.0",
			},
			wantFiles: map[string]string{
				"manifest.yaml": "name: my-app\nversion: 1.0.0",
			},
		},
		{
			name: "renders multiple files in nested directories",
			fsys: fstest.MapFS{
				"assets/crds/crd1.yaml": &fstest.MapFile{
					Data: []byte("apiVersion: {{ .APIVersion }}\nkind: {{ .Kind }}"),
				},
				"assets/controller/deployment.yaml": &fstest.MapFile{
					Data: []byte("replicas: {{ .Replicas }}"),
				},
			},
			sourceDir: "assets",
			templateData: struct {
				APIVersion string
				Kind       string
				Replicas   int
			}{
				APIVersion: "v1",
				Kind:       "CustomResource",
				Replicas:   3,
			},
			wantFiles: map[string]string{
				"crds/crd1.yaml":             "apiVersion: v1\nkind: CustomResource",
				"controller/deployment.yaml": "replicas: 3",
			},
		},
		{
			name: "handles files without template variables",
			fsys: fstest.MapFS{
				"source/static.yaml": &fstest.MapFile{
					Data: []byte("key: value"),
				},
			},
			sourceDir:    "source",
			templateData: nil,
			wantFiles: map[string]string{
				"static.yaml": "key: value",
			},
		},
		{
			name: "handles empty directory",
			fsys: fstest.MapFS{
				"empty/.gitkeep": &fstest.MapFile{
					Data: []byte(""),
				},
			},
			sourceDir:    "empty",
			templateData: nil,
			wantFiles: map[string]string{
				".gitkeep": "",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			destDir := t.TempDir()

			err := RenderManifestsInDirectory(tt.fsys, tt.sourceDir, destDir, tt.templateData)
			if (err != nil) != tt.wantErr {
				t.Errorf("RenderManifestsInDirectory() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if tt.wantErr {
				return
			}

			for relPath, wantContent := range tt.wantFiles {
				fullPath := filepath.Join(destDir, relPath)

				gotContent, err := os.ReadFile(fullPath)
				if err != nil {
					t.Errorf("failed to read output file %s: %v", relPath, err)
					continue
				}

				if string(gotContent) != wantContent {
					t.Errorf("file %s content = %q, want %q", relPath, string(gotContent), wantContent)
				}
			}
		})
	}
}

func TestRenderManifestsInDirectory_InvalidTemplate(t *testing.T) {
	fsys := fstest.MapFS{
		"source/invalid.yaml": &fstest.MapFile{
			Data: []byte("{{ .MissingField }"),
		},
	}

	destDir := t.TempDir()

	err := RenderManifestsInDirectory(fsys, "source", destDir, nil)
	if err == nil {
		t.Error("expected error for invalid template, got nil")
	}
}

func TestRenderManifestsInDirectory_MissingSourceDir(t *testing.T) {
	fsys := fstest.MapFS{}
	destDir := t.TempDir()

	err := RenderManifestsInDirectory(fsys, "nonexistent", destDir, nil)
	if err == nil {
		t.Error("expected error for missing source directory, got nil")
	}
}
