// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestCollectorWorkspaceMembers(t *testing.T) {
	for _, virtual := range []bool{false, true} {
		t.Run(map[bool]string{false: "package root", true: "virtual root"}[virtual], func(t *testing.T) {
			root, home := t.TempDir(), t.TempDir()

			manifest := `[workspace]
members = ["members/*"]
exclude = ["members/excluded"]
[workspace.dependencies]
renamed = { package = "foo", version = "1" }
`
			if !virtual {
				manifest += "[package]\nname = 'racer-dataplane'\n"
			}

			testutil.WriteTree(t, root, map[string]string{
				cratePath + "/Cargo.toml": manifest,
				cratePath + "/members/member/Cargo.toml": `[package]
name = "member"
[dependencies.renamed]
workspace = true
[dev-dependencies.ignored]
path = "missing"
`,
				cratePath + "/members/excluded/Cargo.toml": "[package]\nname = 'excluded'\n[dependencies]\nmissing = '1'\n",
				cratePath + "/Cargo.lock": `[[package]]
name = "racer-dataplane"
version = "0.1.0"
[[package]]
name = "member"
version = "0.1.0"
dependencies = [
 "foo",
 "ignored",
]
[[package]]
name = "foo"
version = "1.0.0"
source = "registry+https://github.com/rust-lang/crates.io-index"
`,
			})
			testutil.WriteTree(t, home, map[string]string{"registry/src/index/foo-1.0.0/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example")})

			c := New(home)
			if err := c.Precheck(root); err != nil {
				t.Fatal(err)
			}

			entries, err := c.Collect(root)
			if err != nil || len(entries) != 1 || entries[0].Dependency != "foo" {
				t.Fatalf("Collect = %#v, %v", entries, err)
			}
		})
	}
}

func TestWorkspaceFailures(t *testing.T) {
	for _, tt := range []struct{ name, member, want string }{
		{"missing manifest", "", "Cargo.toml"},
		{"missing package name", "[dependencies]\n", "no package name"},
		{"missing lock entry", "[package]\nname = 'member'\n", "member package not found"},
		{"missing inherited dependency", "[package]\nname = 'member'\n[dependencies.foo]\nworkspace = true\n", "missing from workspace.dependencies"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			files := map[string]string{
				"Cargo.toml":    "[workspace]\nmembers = ['member']\n",
				"member/README": "placeholder",
			}
			if tt.member != "" {
				files["member/Cargo.toml"] = tt.member
			}

			testutil.WriteTree(t, root, files)

			if _, err := localRegistryVersions(root, ""); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %s", err, tt.want)
			}
		})
	}
}

func TestWorkspaceLocalOnlyPrecheck(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml":        "[workspace]\nmembers = ['member']\n",
		cratePath + "/member/Cargo.toml": "[package]\nname = 'member'\n[dev-dependencies]\nignored = '1'\n",
		cratePath + "/Cargo.lock":        "[[package]]\nname = \"member\"\nversion = \"0.1.0\"\n",
	})

	if err := New(filepath.Join(t.TempDir(), "missing")).Precheck(root); err != nil {
		t.Fatal(err)
	}
}
