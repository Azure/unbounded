// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/notice"
	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

// Keep fixtures independent of crateInputs so omitted members fail collection tests.
var workspaceMembers = []string{
	"runtime", "alloc", "crypto", "verbs", "http", "topology",
	"telemetry", "flow", "control-wire",
	"uds-endpoint", "wire-codec",
}

func TestCollectorCollectHermetic(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": `[package]
name = "racer-dataplane"
[dependencies]
foo = "1"

[build-dependencies]
build-helper = "2"

[target.'cfg(target_os = "linux")'.dependencies]
linux-only = "3"

[dev-dependencies]
test-only = "4"
`,
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "build-helper"
version = "2.0.1"

[[package]]
name = "linux-only"
version = "3.4.5"

[[package]]
name = "test-only"
version = "4.0.0"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "build-helper",
 "foo",
 "linux-only",
 "test-only",
]
`,
	})
	writeCollectorFixtures(t, root, "1.2.3")

	for _, crate := range []string{"foo-1.2.3", "build-helper-2.0.1", "linux-only-3.4.5"} {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/index/" + crate + "/Cargo.toml.orig": "[package]\nlicense = \"MIT\"\n",
			"registry/src/index/" + crate + "/LICENSE":         testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	c := New(cargoHome)
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}

	byDependency := make(map[string]notice.Entry, len(entries))
	for _, entry := range entries {
		byDependency[entry.Dependency] = entry
	}

	for _, dependency := range []string{"build-helper", "foo", "linux-only"} {
		if _, ok := byDependency[dependency]; !ok {
			t.Errorf("dependency %q not collected", dependency)
		}
	}

	if got := byDependency["foo"].License[0].Link; got != "https://docs.rs/crate/foo/1.2.3/source/LICENSE" {
		t.Errorf("license link = %q", got)
	}
}

func TestLockedDirectVersionsUsesQualifiedVersion(t *testing.T) {
	direct := map[string]dependency{"rand": {packageName: "rand"}}
	lock := `[[package]]
name = "rand"
version = "0.8.6"
[[package]]
name = "rand"
version = "0.9.2"
[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "rand 0.8.6",
]
`

	versions, err := lockedDirectVersions(lock, "racer-dataplane", direct)
	if err != nil {
		t.Fatalf("lockedDirectVersions: %v", err)
	}

	if versions["rand"] != "0.8.6" {
		t.Errorf("rand version = %q", versions["rand"])
	}
}

func TestDirectDependenciesResolvesPackageAlias(t *testing.T) {
	direct, err := directDependencies("[dependencies]\nrenamed = { package = \"actual-name\", version = \"1\" }\n")
	if err != nil {
		t.Fatalf("directDependencies: %v", err)
	}

	if got := direct["renamed"].packageName; got != "actual-name" {
		t.Errorf("package name = %q", got)
	}
}

func TestQuotedValue(t *testing.T) {
	for _, tt := range []struct {
		name  string
		value string
		want  string
	}{
		{name: "basic", value: `"../local"`, want: "../local"},
		{name: "literal", value: `'../local'`, want: "../local"},
		{name: "literal backslashes", value: `'C:\new\test'`, want: `C:\new\test`},
		{name: "literal double quote", value: `'local"name'`, want: `local"name`},
		{name: "basic escapes", value: `"\b\t\n\f\r\"\\"`, want: "\b\t\n\f\r\"\\"},
		{name: "unicode", value: `"\u0061\U0001F980"`, want: "a🦀"},
		{name: "unescaped tab", value: "\"a\tb\"", want: "a\tb"},
		{name: "empty literal", value: `''`, want: ""},
		{name: "empty basic", value: `""`, want: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseQuotedValue(tt.value)
			if err != nil || got != tt.want {
				t.Fatalf("parseQuotedValue(%q) = %q, %v; want %q", tt.value, got, err, tt.want)
			}

			if got := quotedValue(tt.value); got != tt.want {
				t.Fatalf("quotedValue(%q) = %q; want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestQuotedValueRejectsInvalidStrings(t *testing.T) {
	for _, value := range []string{
		`local`, `'local`, `"local'`, `"local"junk`, `'one'two'`, `"one"two"`,
		`"\q"`, `"\x41"`, `"\101"`, `"\a"`, `"\v"`, `"\'"`, `"end\"`,
		`"\u123"`, `"\uZZZZ"`, `"\uD800"`, `"\U00110000"`,
		"'line\nbreak'", "\"line\nbreak\"", "'\x7f'", "'\x00'", "'\xff'",
		`'''multiline'''`, `"""multiline"""`,
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseQuotedValue(value); err == nil {
				t.Fatalf("parseQuotedValue(%q) succeeded", value)
			}

			if got := quotedValue(value); got != "" {
				t.Fatalf("quotedValue(%q) = %q; want empty on failure", value, got)
			}
		})
	}
}

func TestDirectDependenciesQuotedFields(t *testing.T) {
	for _, tt := range []struct {
		value string
		path  string
	}{
		{value: `{ features = ["one", "two"], path = '../local,#={}', package = 'actual' } # comment`, path: "../local,#={}"},
		{value: `{ path = "../local\"#,=\u0020dir", package = "\u0061ctual" } # comment`, path: "../local\"#,= dir"},
		{value: `{ path = '..\local\', package = 'actual' } # comment`, path: `..\local\`},
	} {
		t.Run(tt.value, func(t *testing.T) {
			direct, err := directDependencies("[dependencies]\nrenamed = " + tt.value + "\n")
			if err != nil {
				t.Fatal(err)
			}

			if len(direct) != 0 {
				t.Fatalf("local dependency collected as registry dependency: %#v", direct)
			}

			value, _, _ := cutUnquoted(tt.value, '#')
			if got, err := inlineField(value, "path"); err != nil || got != tt.path {
				t.Fatalf("path = %q, %v; want %q", got, err, tt.path)
			}

			if got, err := inlineField(value, "package"); err != nil || got != "actual" {
				t.Fatalf("package = %q, %v; want actual", got, err)
			}
		})
	}
}

func TestDirectDependenciesRejectsInvalidQuotedFields(t *testing.T) {
	for _, value := range []string{`"unterminated`, `'unterminated`, `"bad\q"`, `"bad\uD800"`, `42`, `''`, `""`, `'''multiline'''`} {
		for _, field := range []string{"path", "package"} {
			t.Run(field+"/"+value, func(t *testing.T) {
				_, err := directDependencies("[dependencies]\nlocal = { " + field + " = " + value + " }\n")
				if err == nil || !strings.Contains(err.Error(), "dependency local:") || !strings.Contains(err.Error(), field) {
					t.Fatalf("error = %v; want dependency and %s context", err, field)
				}
			})
		}
	}
}

func TestCollectorPrecheckAndCollectWithoutDependencies(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = 'racer-dataplane'\n[dependencies]\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "racer-dataplane"
version = "0.1.0"
`,
	})

	writeCollectorFixtures(t, root, "1.2.3")
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-loadgen/performance/Cargo.toml": "[package]\nname = 'racer-performance-control'\n",
	})
	cargoHome := t.TempDir()
	testutil.WriteTree(t, cargoHome, map[string]string{"registry/src/.keep": ""})

	c := New(cargoHome)
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck: %v", err)
	}

	entries, err := c.Collect(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Collect = %v, %v; want no entries and no error", entries, err)
	}
}

func TestCollectorPrecheckReportsMissingCache(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\n",
		"cmd/racer-dataplane/Cargo.lock": "version = 4\n",
	})
	writeCollectorFixtures(t, root, "1.2.3")

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cargo fetch") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorRejectsDuplicateRegistrySources(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	writeCollectorFixtures(t, root, "1.2.3")

	for _, registry := range []string{"first", "second"} {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/" + registry + "/foo-1.2.3/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	_, err := New(cargoHome).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "multiple registry source directories") {
		t.Fatalf("Collect error = %v", err)
	}
}

func TestCollectorCollectsMultipleLicenseFiles(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	writeCollectorFixtures(t, root, "1.2.3")
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/LICENSE":        "Choose either LICENSE-APACHE or LICENSE-MIT, included alongside this index.\n",
		"registry/src/index/foo-1.2.3/LICENSE-APACHE": testutil.Apache2License(),
		"registry/src/index/foo-1.2.3/LICENSE-MIT":    testutil.MITLicense("Copyright (c) 2026 Example"),
	})

	entries, err := New(cargoHome).Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 1 || len(entries[0].License) != 2 {
		t.Fatalf("entries = %#v", entries)
	}

	if entries[0].License[0].Name != "Apache License, Version 2.0" || entries[0].License[1].Name != "MIT License" {
		t.Fatalf("licenses = %#v", entries[0].License)
	}
}

func TestLicenseIndexRejectsUnrecognizedLicenseText(t *testing.T) {
	for _, paths := range [][]string{
		{"LICENSE"},
		{"LICENSE", "LICENSE-MIT"},
		{"LICENSE", "COPYING"},
	} {
		if licenseIndex("LICENSE", []byte("Unknown license terms"), paths) {
			t.Fatalf("unrecognized text accepted for %v", paths)
		}
	}
}

func TestCollectorUsesDeclaredLicenseWithoutLicenseFile(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `version = 4

[[package]]
name = "foo"
version = "1.2.3"

[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	writeCollectorFixtures(t, root, "1.2.3")
	testutil.WriteTree(t, cargoHome, map[string]string{
		"registry/src/index/foo-1.2.3/Cargo.toml.orig": "[package]\nlicense = \"MIT OR Apache-2.0\"\n",
	})

	entries, err := New(cargoHome).Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	if len(entries) != 1 || len(entries[0].License) != 2 {
		t.Fatalf("entries = %#v", entries)
	}

	if entries[0].License[0].Name != "MIT License" || entries[0].License[1].Name != "Apache License, Version 2.0" {
		t.Fatalf("licenses = %#v", entries[0].License)
	}
}

func TestCollectorFollowsLocalDependencies(t *testing.T) {
	for _, section := range []string{"dependencies", "build-dependencies", `target.'cfg(unix)'.dependencies`, `target.'cfg(unix)'.build-dependencies`} {
		t.Run(section, func(t *testing.T) {
			root := t.TempDir()
			cargoHome := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-dataplane/Cargo.toml": "[package]\nname = 'racer-dataplane'\n[" + section + "]\nrenamed = { package = 'racer-topology', path = 'topology' }\n[dev-dependencies]\nignored = { path = 'missing' }\n",
				"cmd/racer-dataplane/Cargo.lock": "[[package]]\nname = 'racer-dataplane'\nversion = '0.1.0'\n",
			})
			writeCollectorFixtures(t, root, "1.2.3")

			lock, err := os.ReadFile(filepath.Join(root, "cmd/racer-dataplane/Cargo.lock"))
			if err != nil {
				t.Fatal(err)
			}

			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-dataplane/topology/Cargo.toml":  "[package]\nname = 'topology'\n[" + section + "]\nalias = { package = \"\\u0066oo\", version = '1' }\n[dev-dependencies]\nignored = { path = 'missing' }\n",
				"cmd/racer-loadgen/performance/Cargo.toml": "[package]\nname = 'racer-performance-control'\n",
				"cmd/racer-dataplane/Cargo.lock":           string(lock) + "\n[[package]]\nname = 'topology'\nversion = '0.1.0'\ndependencies = [\n 'foo',\n]\n[[package]]\nname = 'foo'\nversion = '1.2.3'\n",
			})
			testutil.WriteTree(t, cargoHome, map[string]string{
				"registry/src/index/foo-1.2.3/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
			})

			entries, err := New(cargoHome).Collect(root)
			if err != nil || len(entries) != 1 || entries[0].Dependency != "foo" {
				t.Fatalf("Collect = %#v, %v; want only foo", entries, err)
			}
		})
	}
}

func TestCollectorLocalDependencyFailures(t *testing.T) {
	const (
		rootLock = `[[package]]
name = "racer-dataplane"
version = "0.1.0"
`
		topologyLock = `[[package]]
name = "topology"
version = "0.1.0"
dependencies = [
 "sha2",
]
`
		shaLock = `[[package]]
name = "sha2"
version = "0.10.9"
`
	)

	for _, tt := range []struct {
		name     string
		manifest string
		lock     string
		want     string
	}{
		{name: "missing manifest", lock: rootLock + topologyLock, want: "topology/Cargo.toml"},
		{name: "malformed dependency", manifest: "[dependencies]\ninvalid\n", lock: rootLock + topologyLock, want: "invalid dependency line"},
		{name: "invalid path escape", manifest: "[dependencies]\nsha2 = { path = \"bad\\q\" }\n", lock: rootLock + topologyLock, want: "dependency sha2: invalid path"},
		{name: "unterminated literal path", manifest: "[dependencies]\nsha2 = { path = 'unterminated }\n", lock: rootLock + topologyLock, want: "dependency sha2: invalid path"},
		{name: "empty path", manifest: "[dependencies]\nsha2 = { path = '' }\n", lock: rootLock + topologyLock, want: "dependency sha2: empty path"},
		{name: "invalid package escape", manifest: "[dependencies]\nsha2 = { package = \"bad\\q\" }\n", lock: rootLock + topologyLock, want: "dependency sha2: invalid package"},
		{name: "missing local lock entry", manifest: "[dependencies]\n", lock: rootLock, want: "topology package not found"},
		{name: "missing registry lock entry", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock, want: "dependency sha2 has no locked version"},
		{name: "missing dependency edge", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + "[[package]]\nname = \"topology\"\nversion = \"0.1.0\"\n" + shaLock, want: "direct dependency sha2 not found in root lock entry"},
		{name: "ambiguous registry version", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock + shaLock + "[[package]]\nname = \"sha2\"\nversion = \"0.9.9\"\n", want: "dependency sha2 has ambiguous locked versions"},
		{name: "missing registry cache", manifest: "[dependencies]\nsha2 = \"0.10\"\n", lock: rootLock + topologyLock + shaLock, want: "registry source directory not found"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()

			files := map[string]string{
				"cmd/racer-dataplane/Cargo.toml": "[package]\nname = 'racer-dataplane'\n[dependencies]\ntopology = { path = 'topology' }\n",
				"cmd/racer-dataplane/Cargo.lock": tt.lock,
			}
			testutil.WriteTree(t, root, files)
			writeCollectorFixtures(t, root, "1.2.3")

			manifestPath := filepath.Join(root, "cmd/racer-dataplane/topology/Cargo.toml")
			if tt.manifest == "" {
				if err := os.Remove(manifestPath); err != nil {
					t.Fatal(err)
				}
			} else {
				testutil.WriteTree(t, root, map[string]string{
					"cmd/racer-dataplane/topology/Cargo.toml": "[package]\nname = 'topology'\n" + tt.manifest,
				})
			}

			_, err := New(t.TempDir()).Collect(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Collect error = %v; want %q", err, tt.want)
			}

			if tt.name != "missing registry cache" {
				if !strings.HasPrefix(err.Error(), "collecting dependencies for "+manifestPath+": ") {
					t.Fatalf("Collect error = %v; want dependency collection context", err)
				}
			}

			if tt.name == "missing manifest" {
				if !strings.Contains(err.Error(), "resolving "+manifestPath) || !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("Collect error = %v; want wrapped manifest resolution error", err)
				}
			}
		})
	}
}

func writeCollectorFixtures(t *testing.T, root, version string) {
	t.Helper()

	lock, err := os.ReadFile(filepath.Join(root, "cmd/racer-dataplane/Cargo.lock"))
	if err != nil {
		t.Fatal(err)
	}

	for _, member := range workspaceMembers {
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.toml": "[package]\nname = \"racer-" + member + "\"\n",
		})
		lock = append(lock, []byte("\n[[package]]\nname = \"racer-"+member+"\"\nversion = \"0.1.0\"\n")...)
	}

	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.lock": string(lock),
		"cmd/racer-loadgen/performance/Cargo.toml": `[package]
name = "racer-performance-control"
[dependencies]
racer-dataplane = { path = "../../racer-dataplane" }
foo = "1"
`,
		"cmd/racer-loadgen/performance/Cargo.lock": `[[package]]
name = "foo"
version = "` + version + `"
[[package]]
name = "racer-performance-control"
version = "0.1.0"
dependencies = [
 "foo",
 "racer-dataplane",
]
`,
	})
}

func TestCollectorRejectsConflictingDirectVersions(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n[dependencies]\nfoo = \"1\"\n",
		"cmd/racer-dataplane/Cargo.lock": `[[package]]
name = "foo"
version = "1.2.3"
[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "foo",
]
`,
	})
	writeCollectorFixtures(t, root, "1.2.4")

	_, err := New(t.TempDir()).Collect(root)
	if err == nil || !strings.Contains(err.Error(), "conflicting direct versions") {
		t.Fatalf("Collect error = %v", err)
	}
}

func TestManifestPackageName(t *testing.T) {
	for _, tc := range []struct {
		manifest, want string
	}{
		{"[package]\nname = \"racer-performance-control\"\n", "racer-performance-control"},
		{"[package]\nname = 'racer-performance-control' # comment\n", "racer-performance-control"},
		{"[package]\nname = \"racer\\u002dperformance-control\" # comment\n", "racer-performance-control"},
		{"[package]\nname = 'name#literal' # comment\n", "name#literal"},
		{"[dependencies]\nname = \"not-the-root\"\n", ""},
		{"[package]\nname = \"\"\n", ""},
	} {
		got, err := manifestPackageName(tc.manifest)
		if got != tc.want || (err != nil) != (tc.want == "") {
			t.Fatalf("manifestPackageName(%q) = %q, %v", tc.manifest, got, err)
		}
	}
}

func TestLockedDirectVersionsRejectsMissingRoot(t *testing.T) {
	_, err := lockedDirectVersions("[[package]]\nname = \"other\"\n", "racer-dataplane", nil)
	if err == nil || !strings.Contains(err.Error(), "racer-dataplane package not found") {
		t.Fatalf("lockedDirectVersions error = %v", err)
	}
}

func TestCollectorPrecheckRequiresEveryManifest(t *testing.T) {
	root := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n",
		"cmd/racer-dataplane/Cargo.lock": "version = 4\n",
	})

	err := New(t.TempDir()).Precheck(root)
	if err == nil || !strings.Contains(err.Error(), "cmd/racer-loadgen/performance/Cargo.toml") {
		t.Fatalf("Precheck error = %v", err)
	}
}

func TestCollectorCollectWorkspaceMembers(t *testing.T) {
	root := t.TempDir()
	cargoHome := t.TempDir()
	testutil.WriteTree(t, root, map[string]string{
		"cmd/racer-dataplane/Cargo.toml": `[package]
name = "racer-dataplane"
[dependencies]
racer-runtime = { path = "runtime" }
racer-alloc = { path = "alloc" }
racer-crypto = { path = "crypto" }
rdma-verbs = { path = "verbs" }
http1 = { path = "http" }
topology = { path = "topology" }
telemetry = { path = "telemetry" }
flow-control = { path = "flow" }
racer-control-wire = { path = "control-wire" }
uds-endpoint = { path = "uds-endpoint" }
wire-codec = { path = "wire-codec" }
`,
		"cmd/racer-dataplane/Cargo.lock": "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n",
	})
	writeCollectorFixtures(t, root, "1.2.3")

	// Only the members depend directly on these registry packages. Each member
	// selects an exact version from a workspace lock containing two versions.
	lock := "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n"
	want := map[string]string{"foo": "1.2.3"}

	for _, member := range workspaceMembers {
		name := member + "-dep"
		want[name] = "1.2.3"
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.toml": fmt.Sprintf("[package]\nname = %q\n[dependencies]\n%s = \"1\"\n", "racer-"+member, name),
		})
		lock += fmt.Sprintf(`
[[package]]
name = %q
version = "0.1.0"
dependencies = [
 %q,
]
[[package]]
name = %q
version = "1.2.3"
[[package]]
name = %q
version = "2.0.0"
`, "racer-"+member, name+" 1.2.3", name, name)
	}

	testutil.WriteTree(t, root, map[string]string{"cmd/racer-dataplane/Cargo.lock": lock})

	for name, version := range want {
		testutil.WriteTree(t, cargoHome, map[string]string{
			"registry/src/index/" + name + "-" + version + "/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example"),
		})
	}

	c := New(cargoHome)
	if err := c.Precheck(root); err != nil {
		t.Fatalf("Precheck without member locks: %v", err)
	}

	// A stale adjacent member lock must not override the workspace lock.
	for _, member := range workspaceMembers {
		testutil.WriteTree(t, root, map[string]string{
			"cmd/racer-dataplane/" + member + "/Cargo.lock": "invalid stale member lock\n",
		})
	}

	entries, err := c.Collect(root)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got := map[string]string{}

	for _, entry := range entries {
		if len(entry.License) != 1 {
			t.Fatalf("licenses for %s = %#v", entry.Dependency, entry.License)
		}

		got[entry.Dependency] = entry.License[0].Link
	}

	for name, version := range want {
		want[name] = fmt.Sprintf("https://docs.rs/crate/%s/%s/source/LICENSE", name, version)
	}

	if !reflect.DeepEqual(got, want) || len(entries) != len(want) {
		t.Fatalf("entries = %#v, want links %v", entries, want)
	}
}

func TestCollectorRequiresWorkspaceInputs(t *testing.T) {
	inputs := []string{"cmd/racer-dataplane/Cargo.toml", "cmd/racer-dataplane/Cargo.lock"}
	for _, member := range workspaceMembers {
		inputs = append(inputs, "cmd/racer-dataplane/"+member+"/Cargo.toml")
	}

	for _, missing := range inputs {
		t.Run(missing, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-dataplane/Cargo.toml": "[package]\nname = \"racer-dataplane\"\n",
				"cmd/racer-dataplane/Cargo.lock": "[[package]]\nname = \"racer-dataplane\"\nversion = \"0.1.0\"\n",
			})
			writeCollectorFixtures(t, root, "1.2.3")

			if err := os.Remove(filepath.Join(root, missing)); err != nil {
				t.Fatal(err)
			}

			c := New(t.TempDir())
			if err := c.Precheck(root); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Precheck error = %v, want missing %s", err, missing)
			}

			if _, err := c.Collect(root); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("Collect error = %v, want missing %s", err, missing)
			}
		})
	}
}

func TestCollectorFollowsEscapedLocalPaths(t *testing.T) {
	for _, tt := range []struct {
		value string
		path  string
	}{
		{value: `'local,#{}'`, path: "local,#{}"},
		{value: `"local\u0020\U0001F980"`, path: "local 🦀"},
		{value: `"local\"#,dir"`, path: "local\"#,dir"},
		{value: `"local\\dir"`, path: `local\dir`},
	} {
		t.Run(tt.value, func(t *testing.T) {
			root := t.TempDir()
			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-dataplane/Cargo.toml": "[package]\nname = 'racer-dataplane'\n[dependencies]\nlocal = { path = " + tt.value + " } # comment\n",
				"cmd/racer-dataplane/Cargo.lock": `[[package]]
name = "racer-dataplane"
version = "0.1.0"
dependencies = [
 "local",
]
[[package]]
name = "local"
version = "0.1.0"
`,
			})
			writeCollectorFixtures(t, root, "1.2.3")
			testutil.WriteTree(t, root, map[string]string{
				"cmd/racer-loadgen/performance/Cargo.toml": "[package]\nname = 'racer-performance-control'\n",
			})

			// Local crates are excluded from registry notices; known workspace
			// inputs are collected separately, not by traversing these paths.
			if got, err := parseQuotedValue(tt.value); err != nil || got != tt.path {
				t.Fatalf("path = %q, %v; want %q", got, err, tt.path)
			}

			entries, err := New(t.TempDir()).Collect(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("Collect = %v, %v; want no registry entries and no error", entries, err)
			}
		})
	}
}
