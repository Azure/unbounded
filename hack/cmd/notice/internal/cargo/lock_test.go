// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func registryPackage(name, version, deps string) string {
	return fmt.Sprintf("[[package]]\nname = %q\nversion = %q\nsource = 'registry+https://github.com/rust-lang/crates.io-index'\ndependencies = [%s]\n", name, version, deps)
}

func TestRegistryClosure(t *testing.T) {
	lock := registryPackage("foo", "1.0.0", `"bar 2.0.0"`) + registryPackage("bar", "2.0.0", `"baz"`) +
		registryPackage("bar", "1.0.0", "") + registryPackage("baz", "3.0.0", `"foo"`) + registryPackage("dev", "1.0.0", "")

	versions, err := registryClosure(lock, map[string]string{"foo": "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}

	if len(versions) != 3 || versions["bar"] != "2.0.0" || versions["baz"] != "3.0.0" || versions["dev"] != "" {
		t.Fatalf("versions = %#v", versions)
	}
}

func TestRegistryClosureFailures(t *testing.T) {
	for _, tt := range []struct{ name, deps, rest, want string }{
		{"missing package", `"missing"`, "", "no locked version"},
		{"mismatched source", `"bar 1.0.0 (registry+https://other.example/index)"`, registryPackage("bar", "1.0.0", ""), "mismatched locked source"},
		{"missing qualified version", `"bar 9.0.0"`, registryPackage("bar", "1.0.0", ""), "no locked version"},
		{"ambiguous edge", `"bar"`, registryPackage("bar", "1.0.0", "") + registryPackage("bar", "2.0.0", ""), "ambiguous locked versions"},
		{"conflicting reachable versions", `"bar 1.0.0", "bar 2.0.0"`, registryPackage("bar", "1.0.0", "") + registryPackage("bar", "2.0.0", ""), "ambiguous locked versions"},
		{"duplicate sources", `"bar 1.0.0"`, registryPackage("bar", "1.0.0", "") + strings.ReplaceAll(registryPackage("bar", "1.0.0", ""), "crates.io-index", "other-index"), "ambiguous locked versions or sources"},
		{"local edge", `"local"`, "[[package]]\nname = 'local'\nversion = '1.0.0'\n", "unsupported non-registry"},
		{"git edge", `"git"`, "[[package]]\nname = 'git'\nversion = '1.0.0'\nsource = 'git+https://example.com/repo'\n", "unsupported non-registry"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := registryClosure(registryPackage("foo", "1.0.0", tt.deps)+tt.rest, map[string]string{"foo": "1.0.0"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v; want %s", err, tt.want)
			}
		})
	}
}

func TestWorkspaceProductionClosureExcludesDev(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()

	lock := "[[package]]\nname = 'member'\nversion = '0.1.0'\ndependencies = ['normal', 'build', 'target', 'optional', 'dev']\n"
	for _, name := range []string{"normal", "build", "target", "optional"} {
		lock += registryPackage(name, "1.0.0", `"shared"`)
		testutil.WriteTree(t, home, map[string]string{"registry/src/index/" + name + "-1.0.0/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example")})
	}

	lock += registryPackage("shared", "1.0.0", "") + registryPackage("dev", "1.0.0", `"dev-transitive", "shared"`) + registryPackage("dev-transitive", "1.0.0", "")
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml": "[workspace]\nmembers = ['member']\n",
		cratePath + "/member/Cargo.toml": `[package]
name = 'member'
[dependencies]
normal = '1'
optional = { version = '1', optional = true }
[build-dependencies.build]
version = '1'
[target.'cfg(windows)'.dependencies.target]
version = '1'
[dev-dependencies]
dev = '1'
`,
		cratePath + "/Cargo.lock": lock,
	})
	testutil.WriteTree(t, home, map[string]string{"registry/src/index/shared-1.0.0/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example")})

	c := New(home)
	if err := c.Precheck(root); err != nil {
		t.Fatal(err)
	}

	entries, err := c.Collect(root)
	if err != nil || len(entries) != 5 {
		t.Fatalf("Collect = %#v, %v; want four production seeds plus shared", entries, err)
	}

	for _, entry := range entries {
		if strings.HasPrefix(entry.Dependency, "dev") {
			t.Fatalf("collected dev dependency: %s", entry.Dependency)
		}
	}
}
