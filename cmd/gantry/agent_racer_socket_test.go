// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPrepareRacerOriginDirectory(t *testing.T) {
	for _, clientExists := range []bool{false, true} {
		name := "gantry-first"
		if clientExists {
			name = "racer-first"
		}

		t.Run(name, func(t *testing.T) {
			root := t.TempDir()

			client := filepath.Join(root, "gantry", "client")
			if clientExists {
				if err := os.MkdirAll(client, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(client, "socket"), []byte("preserve client"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// Concurrent startup may race with another creator of the cache path.
			var workers sync.WaitGroup
			for range 8 {
				workers.Go(func() {
					if err := prepareRacerOriginDirectory(root); err != nil {
						t.Error(err)
					}
				})
			}

			workers.Wait()

			origin := filepath.Join(root, "gantry", "origin")

			info, err := os.Lstat(origin)
			if err != nil || !info.IsDir() || info.Mode().Perm()&0o700 != 0o700 || info.Mode().Perm()&0o022 != 0 {
				t.Fatalf("origin directory: %v, %v", info, err)
			}

			if clientExists {
				data, err := os.ReadFile(filepath.Join(client, "socket"))
				if err != nil || string(data) != "preserve client" {
					t.Fatalf("client changed: %q, %v", data, err)
				}
			} else if _, err := os.Lstat(client); !os.IsNotExist(err) {
				t.Fatalf("created client endpoint: %v", err)
			}

			if err := os.Chmod(origin, 0o750); err != nil {
				t.Fatal(err)
			}

			socket := filepath.Join(origin, "socket")
			if err := os.WriteFile(socket, []byte("preserve origin"), 0o600); err != nil {
				t.Fatal(err)
			}

			if err := prepareRacerOriginDirectory(root); err != nil {
				t.Fatal(err)
			}

			info, err = os.Lstat(origin)
			if err != nil || info.Mode().Perm() != 0o750 {
				t.Fatalf("existing directory mode changed: %v, %v", info, err)
			}

			data, err := os.ReadFile(socket)
			if err != nil || string(data) != "preserve origin" {
				t.Fatalf("origin changed: %q, %v", data, err)
			}
		})
	}
}

func TestPrepareRacerOriginDirectoryRejectsUnsafePaths(t *testing.T) {
	for _, component := range []string{"mount", "gantry", "origin"} {
		for _, kind := range []string{"file", "symlink", "dangling-symlink"} {
			t.Run(component+"/"+kind, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "mount")

				path := root
				if component != "mount" {
					path = filepath.Join(path, "gantry")
				}

				if component == "origin" {
					path = filepath.Join(path, "origin")
				}

				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}

				target := t.TempDir()

				var err error

				switch kind {
				case "file":
					err = os.WriteFile(path, []byte("preserve"), 0o600)
				case "symlink":
					err = os.Symlink(target, path)
				case "dangling-symlink":
					err = os.Symlink(filepath.Join(target, "missing"), path)
				}

				if err != nil {
					t.Fatal(err)
				}

				if err := prepareRacerOriginDirectory(root); err == nil {
					t.Fatal("accepted unsafe directory")
				}

				entries, err := os.ReadDir(target)
				if err != nil || len(entries) != 0 {
					t.Fatalf("modified symlink target: %v, %v", entries, err)
				}
			})
		}
	}

	t.Run("missing-mount", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "missing")
		if err := prepareRacerOriginDirectory(root); !os.IsNotExist(err) {
			t.Fatalf("expected missing mount error, got %v", err)
		}

		if _, err := os.Lstat(root); !os.IsNotExist(err) {
			t.Fatalf("created missing mount: %v", err)
		}
	})
}
