// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestVerifyAndSeedDirectory(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	writeModelCache(t, source, map[string]string{
		"config.json":           "{}\n",
		"model.safetensors":     "weights",
		"tokenizer_config.json": "{}\n",
	})

	manifest, err := os.ReadFile(filepath.Join(source, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	manifestDigest := digestBytes(manifest)
	if err := verifyDirectory(source, manifestDigest); err != nil {
		t.Fatalf("verifyDirectory() error = %v", err)
	}

	target := filepath.Join(t.TempDir(), "model")
	if err := seedDirectory(source, target, manifestDigest, "pod-uid"); err != nil {
		t.Fatalf("seedDirectory() error = %v", err)
	}

	if err := seedDirectory(source, target, manifestDigest, "second-pod"); err != nil {
		t.Fatalf("idempotent seedDirectory() error = %v", err)
	}

	if err := verifyDirectory(target, manifestDigest); err != nil {
		t.Fatalf("verify seeded directory: %v", err)
	}
}

func TestVerifyDirectoryRejectsUnexpectedOrModifiedFiles(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		mutate func(t *testing.T, directory string)
	}{
		{
			name: "unexpected file",
			mutate: func(t *testing.T, directory string) {
				t.Helper()

				if err := os.WriteFile(filepath.Join(directory, "extra"), []byte("extra"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "modified file",
			mutate: func(t *testing.T, directory string) {
				t.Helper()

				if err := os.Chmod(filepath.Join(directory, "config.json"), 0o644); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(directory, "config.json"), []byte("changed"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unexpected directory",
			mutate: func(t *testing.T, directory string) {
				t.Helper()

				if err := os.Mkdir(filepath.Join(directory, "extra"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "symbolic link",
			mutate: func(t *testing.T, directory string) {
				t.Helper()

				if err := os.Symlink("config.json", filepath.Join(directory, "extra")); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			directory := t.TempDir()
			writeModelCache(t, directory, map[string]string{"config.json": "{}\n"})

			manifest, err := os.ReadFile(filepath.Join(directory, manifestName))
			if err != nil {
				t.Fatal(err)
			}

			tt.mutate(t, directory)

			if err := verifyDirectory(directory, digestBytes(manifest)); err == nil {
				t.Fatal("verifyDirectory() accepted a modified cache")
			}
		})
	}
}

func TestSeedDirectoryRejectsInvalidInputs(t *testing.T) {
	t.Parallel()

	source := t.TempDir()
	writeModelCache(t, source, map[string]string{"config.json": "{}\n"})

	manifest, err := os.ReadFile(filepath.Join(source, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	manifestDigest := digestBytes(manifest)

	for _, tt := range []struct {
		name    string
		seedUID string
		prepare func(t *testing.T, target string)
	}{
		{
			name:    "invalid seed UID",
			seedUID: "../unsafe",
		},
		{
			name:    "existing invalid target",
			seedUID: "pod-uid",
			prepare: func(t *testing.T, target string) {
				t.Helper()

				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(target, "unexpected"), []byte("bad"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:    "existing temporary directory",
			seedUID: "pod-uid",
			prepare: func(t *testing.T, target string) {
				t.Helper()

				if err := os.MkdirAll(target+".seed-pod-uid", 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			target := filepath.Join(t.TempDir(), "model")
			if tt.prepare != nil {
				tt.prepare(t, target)
			}

			if err := seedDirectory(source, target, manifestDigest, tt.seedUID); err == nil {
				t.Fatal("seedDirectory() accepted invalid input")
			}
		})
	}
}

func TestPublishDirectoryRemovesLosingConcurrentSeed(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	target := filepath.Join(parent, "model")

	temporary := target + ".seed-loser"
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(temporary, 0o755); err != nil {
		t.Fatal(err)
	}

	files := map[string]string{"config.json": "{}\n"}
	writeModelCache(t, target, files)
	writeModelCache(t, temporary, files)

	manifest, err := os.ReadFile(filepath.Join(target, manifestName))
	if err != nil {
		t.Fatal(err)
	}

	if err := publishDirectory(temporary, target, digestBytes(manifest)); err != nil {
		t.Fatalf("publishDirectory() error = %v", err)
	}

	if _, err := os.Stat(temporary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("losing temporary directory still exists: %v", err)
	}
}

func TestVerifyDirectoryRejectsManifestDigestMismatch(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()
	writeModelCache(t, directory, map[string]string{"config.json": "{}\n"})

	if err := verifyDirectory(directory, "sha256:"+string(make([]byte, 64))); err == nil {
		t.Fatal("verifyDirectory() accepted the wrong manifest digest")
	}
}

func TestReadManifestRejectsUnsortedPaths(t *testing.T) {
	t.Parallel()

	directory := t.TempDir()

	content := []byte(
		"z\tsha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t1\n" +
			"a\tsha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\t1\n",
	)
	if err := os.WriteFile(filepath.Join(directory, manifestName), content, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := readManifest(directory, digestBytes(content)); err == nil {
		t.Fatal("readManifest() accepted unsorted paths")
	}
}

func TestReadManifestRejectsNonCanonicalFormatting(t *testing.T) {
	t.Parallel()

	for _, content := range []string{
		"config.json\tsha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t1",
		"nested/config.json\tsha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\t1\n",
	} {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, manifestName), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}

		if _, err := readManifest(directory, digestBytes([]byte(content))); err == nil {
			t.Fatal("readManifest() accepted non-canonical formatting")
		}
	}
}

func writeModelCache(t *testing.T, directory string, files map[string]string) {
	t.Helper()

	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}

	slices.Sort(paths)

	var manifest string

	for _, path := range paths {
		content := []byte(files[path])
		if err := os.WriteFile(filepath.Join(directory, path), content, 0o444); err != nil {
			t.Fatal(err)
		}

		manifest += fmt.Sprintf("%s\t%s\t%d\n", path, digestBytes(content), len(content))
	}

	if err := os.WriteFile(filepath.Join(directory, manifestName), []byte(manifest), 0o444); err != nil {
		t.Fatal(err)
	}
}
