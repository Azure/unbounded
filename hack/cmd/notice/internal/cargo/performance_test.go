// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cargo

import (
	"strings"
	"testing"

	"github.com/Azure/unbounded/hack/cmd/notice/internal/testutil"
)

func TestPerformanceRequiresCompleteInputs(t *testing.T) {
	for _, file := range []string{"Cargo.toml", "Cargo.lock"} {
		t.Run(file, func(t *testing.T) {
			root := workspaceFixture(t)
			testutil.WriteTree(t, root, map[string]string{performancePath + "/" + file: ""})

			if _, err := allVersions(root); err == nil || !strings.Contains(err.Error(), "missing "+performancePath) {
				t.Fatalf("allVersions error = %v; want incomplete performance root", err)
			}
		})
	}
}

func TestPerformanceLocalPathsMustUseWorkspaceMembers(t *testing.T) {
	for _, tc := range []struct{ name, path, want string }{
		{"member", "../../racer-dataplane/member", ""},
		{"undeclared", "../../racer-dataplane/undeclared", "must be a declared workspace member"},
		{"outside", "../../../outside", "escapes dataplane workspace"},
		{"missing", "../../racer-dataplane/missing", "no such file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := workspaceFixture(t)
			testutil.WriteTree(t, root, map[string]string{
				performancePath + "/Cargo.toml":      "[package]\nname='performance'\n[dependencies]\nlocal={path='" + tc.path + "'}\n",
				performancePath + "/Cargo.lock":      "[[package]]\nname='performance'\nversion='0.1.0'\n",
				cratePath + "/undeclared/Cargo.toml": "[package]\nname='undeclared'\n",
				"outside/Cargo.toml":                 "[package]\nname='outside'\n",
			})

			versions, err := allVersions(root)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("allVersions error = %v; want %s", err, tc.want)
				}
			} else if err != nil || versions["foo"] != "1.2.3" {
				t.Fatalf("allVersions = %v, %v; want workspace dependencies", versions, err)
			}
		})
	}
}
