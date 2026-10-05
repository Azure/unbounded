// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import "testing"

func TestDirectDependenciesTableSyntax(t *testing.T) {
	for _, section := range []string{"dependencies", "build-dependencies", `target.'cfg(target_os = "linux")'.dependencies`, `target.'cfg(unix)'.build-dependencies`} {
		t.Run(section, func(t *testing.T) {
			deps, err := directDependencies("[" + section + ".renamed]\npackage = 'actual'\npath = 'local#path'\noptional = true\n[dev-dependencies.ignored]\npath = 'missing'\n[target.'cfg(unix)'.dev-dependencies.ignored]\nversion = '1'\n")
			if err != nil {
				t.Fatal(err)
			}

			if len(deps) != 1 || deps["renamed"] != (dependency{packageName: "actual", path: "local#path"}) {
				t.Fatalf("dependencies = %#v", deps)
			}
		})
	}
}

func TestDirectDependenciesInvalidTables(t *testing.T) {
	for _, data := range []string{
		"[dependencies.foo]\npath = 42\n",
		"[dependencies.foo]\npackage = false\n",
		"[dependencies.foo]\nversion = [\n",
		"[dependencies]\nfoo = true\n",
		"[dependencies.foo]\npath = 'one'\n[build-dependencies.foo]\npath = 'two'\n",
	} {
		t.Run(data, func(t *testing.T) {
			if _, err := directDependencies(data); err == nil {
				t.Fatal("expected invalid dependency error")
			}
		})
	}
}
