// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package release

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestKrewManifestChecksums(t *testing.T) {
	script := releaseWorkflowStep(t, "finalize", "Render krew manifest")
	script = strings.ReplaceAll(script, "${{ needs.version.outputs.tag }}", "v0.9.0")
	script = strings.ReplaceAll(script, "${{ github.repository }}", "Azure/unbounded")
	archives := []string{"linux-amd64", "linux-arm64", "darwin-amd64", "darwin-arm64"}

	for _, missing := range append([]string{""}, archives...) {
		t.Run("missing="+missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, subdir := range []string{"bin", "dist", "hack"} {
				if err := os.Mkdir(filepath.Join(dir, subdir), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			for _, arch := range archives {
				if arch != missing {
					path := filepath.Join(dir, "dist", "kubectl-unbounded-"+arch+".tar.gz")
					if err := os.WriteFile(path, []byte(arch), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := os.WriteFile(filepath.Join(dir, "hack", "krew-manifest.yaml"), []byte("template"), 0o644); err != nil {
				t.Fatal(err)
			}

			stub := "#!/usr/bin/env bash\nprintf '%s\\n' \"$SHA_LINUX_AMD64\" \"$SHA_LINUX_ARM64\" \"$SHA_DARWIN_AMD64\" \"$SHA_DARWIN_ARM64\"\n"
			if err := os.WriteFile(filepath.Join(dir, "bin", "envsubst"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("bash", "-e", "-c", script)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))

			output, err := cmd.CombinedOutput()
			if (err != nil) != (missing != "") {
				t.Fatalf("render error = %v, output:\n%s", err, output)
			}

			manifest, err := os.ReadFile(filepath.Join(dir, "dist", "unbounded.yaml"))
			if missing != "" {
				if !os.IsNotExist(err) {
					t.Fatalf("missing archive still rendered manifest: %s, %v", manifest, err)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			var want strings.Builder
			for _, arch := range archives {
				fmt.Fprintf(&want, "%x\n", sha256.Sum256([]byte(arch)))
			}

			if string(manifest) != want.String() {
				t.Fatalf("rendered checksums:\n%s\nwant:\n%s", manifest, want.String())
			}
		})
	}
}
