// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

const workspaceLock = `version = 4
[[package]]
name = "racer-dataplane"
version = "0.1.0"
[[package]]
name = "member"
version = "0.1.0"
dependencies = [
 "foo 1.2.3",
]
[[package]]
name = "foo"
version = "1.2.3"
[[package]]
name = "foo"
version = "2.0.0"
`

func workspaceFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/Cargo.toml":        "[package]\nname = 'racer-dataplane'\n[workspace]\nmembers = [\n '.', # root\n 'member',\n]\n",
		cratePath + "/Cargo.lock":        workspaceLock,
		cratePath + "/member/Cargo.toml": "[package]\nname = 'member'\n[dependencies]\nfoo = '1'\n",
		cratePath + "/member/Cargo.lock": "invalid stale lock",
	})

	return root
}

func TestPartialWorkspaceCollection(t *testing.T) {
	root := workspaceFixture(t)
	home := t.TempDir()
	testutil.WriteTree(t, home, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE": testutil.MITLicense("Copyright Example"),
	})

	c := New(home)
	if err := c.Precheck(root); err != nil {
		t.Fatal(err)
	}

	entries, err := c.Collect(root)
	if err != nil || len(entries) != 1 || entries[0].Dependency != "foo" || !strings.Contains(entries[0].License[0].Link, "/1.2.3/") {
		t.Fatalf("Collect = %#v, %v", entries, err)
	}
}

func TestWorkspaceFailures(t *testing.T) {
	for _, tc := range []struct{ name, manifest, lock, want string }{
		{"missing member", "[workspace]\nmembers=['absent']", workspaceLock, "absent"},
		{"glob", "[workspace]\nmembers=['*']", workspaceLock, "explicit local"},
		{"escape", "[workspace]\nmembers=['../elsewhere']", workspaceLock, "explicit local"},
		{"malformed", "[workspace]\nmembers=[", workspaceLock, "parsing"},
		{"exclude", "[workspace]\nexclude=['member']", workspaceLock, "exclude"},
		{"unlisted path", "[dependencies]\nlocal={path='member'}", workspaceLock, "declared workspace member"},
		{"missing root lock entry", "[workspace]\nmembers=['member']", "version=4\n", "package not found"},
		{"missing member lock entry", "[workspace]\nmembers=['member']", "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n", "member package not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceFixture(t)
			testutil.WriteTree(t, root, map[string]string{cratePath + "/Cargo.toml": tc.manifest, cratePath + "/Cargo.lock": tc.lock})

			c := New(t.TempDir())
			for _, err := range []error{c.Precheck(root), func() error { _, err := c.Collect(root); return err }()} {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want %s", err, tc.want)
				}
			}
		})
	}
}

func TestWorkspaceMissingAndInvalidMembers(t *testing.T) {
	for _, kind := range []string{"missing", "directory", "dangling", "loop", "no package name"} {
		t.Run(kind, func(t *testing.T) {
			root := workspaceFixture(t)

			path := filepath.Join(root, cratePath, "member/Cargo.toml")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}

			var err error

			switch kind {
			case "directory":
				err = os.Mkdir(path, 0o755)
			case "dangling":
				err = os.Symlink("absent", path)
			case "loop":
				err = os.Symlink("Cargo.toml", path)
			case "no package name":
				testutil.WriteTree(t, root, map[string]string{cratePath + "/member/Cargo.toml": "[dependencies]\n"})
			}

			if err != nil {
				t.Fatal(err)
			}

			c := New(t.TempDir())
			if err := c.Precheck(root); err == nil {
				t.Fatal("Precheck accepted invalid member")
			}

			if _, err := c.Collect(root); err == nil {
				t.Fatal("Collect accepted invalid member")
			}
		})
	}
}

func TestWorkspaceDeduplicationAndConflicts(t *testing.T) {
	for _, version := range []string{"1.2.3", "2.0.0"} {
		t.Run(version, func(t *testing.T) {
			root := workspaceFixture(t)
			lock := strings.Replace(workspaceLock, "name = \"racer-dataplane\"\nversion = \"0.1.0\"", "name = \"racer-dataplane\"\nversion = \"0.1.0\"\ndependencies = [\n \"foo "+version+"\",\n]", 1)
			testutil.WriteTree(t, root, map[string]string{
				cratePath + "/Cargo.toml": "[workspace]\nmembers=['member']\n[dependencies]\nfoo='1'\nlocal={path='member'}\n",
				cratePath + "/Cargo.lock": lock,
			})

			versions, err := workspaceVersions(root)
			if version == "2.0.0" {
				if err == nil || !strings.Contains(err.Error(), "conflicting") {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || len(versions) != 1 || versions["foo"] != version {
				t.Fatalf("versions = %v, %v", versions, err)
			}
		})
	}
}

func TestDependencyTOMLSyntax(t *testing.T) {
	for _, section := range []string{"dependencies", "build-dependencies", "target.'cfg(unix)'.dependencies", "target.'cfg(unix)'.build-dependencies"} {
		direct, err := directDependencies("[" + section + "]\nalias={package='foo', version='1', optional=true}\nlocal={path='local,#{}'}\n[dev-dependencies]\nignored='2'\n")
		if err != nil || direct["alias"].packageName != "foo" || direct["path:local,#{}"].localPath != "local,#{}" || direct["ignored"].packageName != "" {
			t.Fatalf("direct = %v, %v", direct, err)
		}
	}

	for _, declaration := range []string{"{path=42}", "{path=''}", "{package=42}", "{workspace=true}", "{git='https://example.com/a'}", "42", "{path='unterminated}"} {
		if _, err := directDependencies("[dependencies]\na=" + declaration); err == nil {
			t.Fatalf("accepted %s", declaration)
		}
	}
}

func TestLicenseIndex(t *testing.T) {
	paths := []string{"LICENSE", "LICENSE-MIT", "LICENSE-APACHE"}
	if !licenseIndex("LICENSE", []byte("See LICENSE-MIT or LICENSE-APACHE"), paths) {
		t.Fatal("rejected index")
	}

	if licenseIndex("LICENSE", []byte("Unknown terms"), paths) {
		t.Fatal("accepted unknown terms")
	}

	if licenseIndex("LICENSE", []byte("See LICENSE-MIT"), paths) {
		t.Fatal("accepted incomplete index")
	}
}

func TestCollectorLicenseIndexCompanions(t *testing.T) {
	for _, valid := range []bool{true, false} {
		t.Run(map[bool]string{true: "classified companions", false: "unknown companion"}[valid], func(t *testing.T) {
			root := workspaceFixture(t)
			home := t.TempDir()

			companion := testutil.MITLicense("Copyright Example")
			if !valid {
				companion = "Unknown license terms"
			}

			testutil.WriteTree(t, home, map[string]string{
				"registry/src/index/foo-1.2.3/LICENSE":        "Choose LICENSE-MIT or LICENSE-APACHE.",
				"registry/src/index/foo-1.2.3/LICENSE-MIT":    companion,
				"registry/src/index/foo-1.2.3/LICENSE-APACHE": testutil.Apache2License(),
			})

			entries, err := New(home).Collect(root)
			if !valid {
				if err == nil || !strings.Contains(err.Error(), "classifying") {
					t.Fatalf("error = %v", err)
				}
			} else if err != nil || len(entries) != 1 || len(entries[0].License) != 2 {
				t.Fatalf("entries = %v, %v", entries, err)
			}
		})
	}
}

func TestDependencyFreeWorkspaceNeedsNoCache(t *testing.T) {
	root := workspaceFixture(t)
	testutil.WriteTree(t, root, map[string]string{
		cratePath + "/member/Cargo.toml": "[package]\nname='member'\n",
	})

	c := New(filepath.Join(t.TempDir(), "absent"))
	if err := c.Precheck(root); err != nil {
		t.Fatal(err)
	}

	if entries, err := c.Collect(root); err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v", entries, err)
	}
}

func TestWorkspaceMemberRequiresRegistryCache(t *testing.T) {
	root := workspaceFixture(t)

	c := New(filepath.Join(t.TempDir(), "absent"))
	if err := c.Precheck(root); err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck = %v", err)
	}
}

func TestWorkspaceRejectsEscapingSymlink(t *testing.T) {
	root := workspaceFixture(t)

	path := filepath.Join(root, cratePath, "member/Cargo.toml")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	external := t.TempDir()
	testutil.WriteTree(t, external, map[string]string{"Cargo.toml": "[package]\nname='member'\n"})

	if err := os.Symlink(filepath.Join(external, "Cargo.toml"), path); err != nil {
		t.Fatal(err)
	}

	if _, err := workspaceVersions(root); err == nil || !strings.Contains(err.Error(), "escapes workspace") {
		t.Fatalf("error = %v", err)
	}
}

func TestDependencySubtablesAndTargetAliases(t *testing.T) {
	direct, err := directDependencies(`[dependencies.alias]
package = "foo"
version = "1"
[target.'cfg(unix)'.dependencies.alias]
package = "bar"
version = "1"
[dev-dependencies]
ignored = { path = "missing" }
`)
	if err != nil || !containsPackage(direct, "foo") || !containsPackage(direct, "bar") || containsPackage(direct, "ignored") {
		t.Fatalf("direct = %v, %v", direct, err)
	}
}

func TestLockedPackageFailures(t *testing.T) {
	for _, tc := range []struct{ lock, want string }{
		{"[[package]]\nname='racer-dataplane'\nversion='1'\ndependencies=['foo 1']\n", "no lock entry"},
		{"[[package]]\nname='racer-dataplane'\nversion='1'\ndependencies=['foo']\n", "no locked version"},
		{"[[package]]\nname='racer-dataplane'\nversion='1'\n", "not found in root lock entry"},
		{"[[package]]\nname='racer-dataplane'\n[[package]]\nname='racer-dataplane'\n", "ambiguous workspace"},
		{"[[package]]\nname='racer-dataplane'\ndependencies=['foo']\n[[package]]\nname='foo'\nversion='1'\n[[package]]\nname='foo'\nversion='2'\n", "ambiguous locked"},
	} {
		_, err := lockedDirectVersions(tc.lock, map[string]dependency{"foo": {packageName: "foo"}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("error = %v, want %s", err, tc.want)
		}
	}
}
