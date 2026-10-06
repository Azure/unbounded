// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

// Exact LICENSE text from rustls-pemfile 2.2.0, independent of the recognizer.
const pemfileIndexFixture = `rustls-pemfile is distributed under the following three licenses:

- Apache License version 2.0.
- MIT license.
- ISC license.

These are included as LICENSE-APACHE, LICENSE-MIT and LICENSE-ISC
respectively.  You may use this software under the terms of any
of these licenses, at your option.
`

const iscFixture = `ISC License (ISC)
Copyright (c) 2026 Example ISC Owner

Permission to use, copy, modify, and/or distribute this software for
any purpose with or without fee is hereby granted, provided that the
above copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL
WARRANTIES WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED
WARRANTIES OF MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE
AUTHOR BE LIABLE FOR ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL
DAMAGES OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR
PROFITS, WHETHER IN AN ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS
ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR PERFORMANCE OF
THIS SOFTWARE.
`

func TestCollectorLicenseIndexRejectsUnknownTermsAndMissingReferences(t *testing.T) {
	for _, tc := range []struct{ name, index, missing, extra string }{
		{name: "unknown terms with filenames", index: "Custom restrictions apply. See LICENSE-APACHE, LICENSE-MIT and LICENSE-ISC."},
		{name: "appended restriction", index: pemfileIndexFixture + "Commercial use is forbidden.\n"},
		{name: "prepended restriction", index: "Commercial use is forbidden.\n" + pemfileIndexFixture},
		{name: "missing ISC", index: pemfileIndexFixture, missing: "LICENSE-ISC"},
		{name: "missing Apache", index: pemfileIndexFixture, missing: "LICENSE-APACHE"},
		{name: "missing MIT", index: pemfileIndexFixture, missing: "LICENSE-MIT"},
		{name: "extra absent reference", index: pemfileIndexFixture + "See LICENSE-CUSTOM for additional terms."},
		{name: "unrecognized additional file", index: pemfileIndexFixture, extra: "Unknown custom terms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceFixture(t)
			home := t.TempDir()
			files := map[string]string{
				"LICENSE":        tc.index,
				"LICENSE-APACHE": testutil.Apache2License(),
				"LICENSE-MIT":    testutil.MITLicense("Copyright (c) 2026 Example"),
				"LICENSE-ISC":    iscFixture,
			}
			delete(files, tc.missing)

			if tc.extra != "" {
				files["LICENSE-CUSTOM"] = tc.extra
			}

			for name, text := range files {
				testutil.WriteTree(t, home, map[string]string{"registry/src/index/foo-1.2.3/" + name: text})
			}

			if _, err := New(home).Collect(root); err == nil || !strings.Contains(err.Error(), "classifying") {
				t.Fatalf("Collect error = %v, want classification failure", err)
			}
		})
	}
}

func TestCollectorLicenseIndexRetainsAttribution(t *testing.T) {
	const (
		indexOwner = "Copyright (c) 2026 Index Only Owner"
		mitOwner   = "Copyright (c) 2026 MIT Only Owner"
	)

	for _, subject := range []string{"rustls-pemfile", "Rustls"} {
		t.Run(subject, func(t *testing.T) {
			root := workspaceFixture(t)
			home := t.TempDir()
			testutil.WriteTree(t, home, map[string]string{
				"registry/src/index/foo-1.2.3/LICENSE":        indexOwner + "\n\n" + strings.Replace(pemfileIndexFixture, "rustls-pemfile", subject, 1),
				"registry/src/index/foo-1.2.3/LICENSE-APACHE": testutil.Apache2License(),
				"registry/src/index/foo-1.2.3/LICENSE-MIT":    testutil.MITLicense(mitOwner),
				"registry/src/index/foo-1.2.3/LICENSE-ISC":    iscFixture,
			})

			entries, err := New(home).Collect(root)
			if err != nil || len(entries) != 1 {
				t.Fatalf("Collect = %v, %v", entries, err)
			}

			entry := entries[0]
			if !slices.Contains(entry.Copyright, indexOwner) || !slices.Contains(entry.Copyright, mitOwner) || !slices.Contains(entry.Copyright, "Copyright (c) 2026 Example ISC Owner") {
				t.Fatalf("lost attribution: %v", entry.Copyright)
			}

			if len(entry.License) != 3 {
				t.Fatalf("licenses = %v", entry.License)
			}

			for _, item := range entry.License {
				if strings.HasSuffix(item.Link, "/LICENSE") {
					t.Fatalf("index emitted as license: %v", item)
				}
			}
		})
	}
}

func TestCollectorDevelopmentPathsRequireDeclaredMembers(t *testing.T) {
	for _, section := range []string{"dev-dependencies", "target.'cfg(unix)'.dev-dependencies"} {
		for _, fromMember := range []bool{false, true} {
			for _, declared := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/fromMember=%t/declared=%t", section, fromMember, declared), func(t *testing.T) {
					root := workspaceFixture(t)

					members := "['.', 'member']"
					if declared {
						members = "['.', 'member', 'helper']"
					}

					rootManifest := "[package]\nname='racer-dataplane'\n[workspace]\nmembers=" + members + "\n"
					memberManifest := "[package]\nname='member'\n[dependencies]\nfoo='1'\n"

					path := "helper"
					if fromMember {
						path = "../helper"
					}

					dev := "[" + section + "]\nhelper={path='" + path + "'}\nregistry-dev='9'\n"
					if fromMember {
						memberManifest += dev
					} else {
						rootManifest += dev
					}

					lock := workspaceLock + "\n[[package]]\nname='helper'\nversion='0.1.0'\ndependencies=['helper-dep', 'registry-dev']\n[[package]]\nname='helper-dep'\nversion='1.0.0'\n[[package]]\nname='registry-dev'\nversion='9.0.0'\n"
					if fromMember {
						lock = strings.Replace(lock, " \"foo 1.2.3\",", " \"foo 1.2.3\",\n \"helper\",\n \"registry-dev\",", 1)
					} else {
						lock = strings.Replace(lock, "name = \"racer-dataplane\"\nversion = \"0.1.0\"", "name = \"racer-dataplane\"\nversion = \"0.1.0\"\ndependencies = ['helper', 'registry-dev']", 1)
					}

					testutil.WriteTree(t, root, map[string]string{
						cratePath + "/Cargo.toml":        rootManifest,
						cratePath + "/Cargo.lock":        lock,
						cratePath + "/member/Cargo.toml": memberManifest,
						cratePath + "/helper/Cargo.toml": "[package]\nname='helper'\n[dependencies]\nhelper-dep='1'\n[dev-dependencies]\nregistry-dev='9'\n",
					})

					home := t.TempDir()
					for _, name := range []string{"foo-1.2.3", "helper-dep-1.0.0"} {
						testutil.WriteTree(t, home, map[string]string{"registry/src/index/" + name + "/LICENSE": testutil.MITLicense("Copyright (c) 2026 Example")})
					}

					c := New(home)
					precheck := c.Precheck(root)

					entries, err := c.Collect(root)
					if !declared {
						for _, failure := range []error{precheck, err} {
							if failure == nil || !strings.Contains(failure.Error(), "declared workspace member") {
								t.Fatalf("error = %v", failure)
							}
						}

						return
					}

					if precheck != nil || err != nil || len(entries) != 2 {
						t.Fatalf("Precheck = %v; Collect = %v, %v", precheck, entries, err)
					}

					names := []string{entries[0].Dependency, entries[1].Dependency}
					if !slices.Contains(names, "foo") || !slices.Contains(names, "helper-dep") {
						t.Fatalf("dependencies = %v", names)
					}
				})
			}
		}
	}
}
