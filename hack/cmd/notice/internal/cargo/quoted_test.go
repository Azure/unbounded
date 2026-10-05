// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

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

			if len(direct) != 1 || direct["renamed"] != (dependency{packageName: "actual", path: tt.path}) {
				t.Fatalf("direct = %#v; want renamed local dependency", direct)
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
				cratePath + "/Cargo.toml":                 "[dependencies]\nlocal = { path = " + tt.value + " } # comment\n",
				cratePath + "/" + tt.path + "/Cargo.toml": "[dependencies]\n",
				cratePath + "/Cargo.lock": `[[package]]
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

			c := New(filepath.Join(t.TempDir(), "nonexistent-cache"))
			if err := c.Precheck(root); err != nil {
				t.Fatal(err)
			}

			entries, err := c.Collect(root)
			if err != nil || len(entries) != 0 {
				t.Fatalf("Collect = %v, %v; want no registry entries and no error", entries, err)
			}
		})
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

func TestCollectorLicenseIndex(t *testing.T) {
	for _, invalidCompanion := range []bool{false, true} {
		t.Run(map[bool]string{false: "valid companions", true: "invalid companion"}[invalidCompanion], func(t *testing.T) {
			home := t.TempDir()

			mit := testutil.MITLicense("Copyright (c) 2026 Example")
			if invalidCompanion {
				mit = "Unknown license terms"
			}

			testutil.WriteTree(t, home, map[string]string{
				"registry/src/index/foo-1.2.3/LICENSE":        "Choose either LICENSE-APACHE or LICENSE-MIT, included alongside this index.\n",
				"registry/src/index/foo-1.2.3/LICENSE-APACHE": testutil.Apache2License(),
				"registry/src/index/foo-1.2.3/LICENSE-MIT":    mit,
			})

			entry, err := New(home).buildEntry("foo", "1.2.3")
			if invalidCompanion {
				if err == nil || !strings.Contains(err.Error(), "classifying") {
					t.Fatalf("error = %v; want invalid companion rejection", err)
				}

				return
			}

			if err != nil || len(entry.License) != 2 {
				t.Fatalf("entry = %#v, %v; want two licenses", entry, err)
			}

			if entry.License[0].Name != "Apache License, Version 2.0" || entry.License[1].Name != "MIT License" {
				t.Fatalf("licenses = %#v", entry.License)
			}
		})
	}
}
