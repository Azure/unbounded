// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// hardenAPTCalls returns the host.HardenAPT* constructors called inside the
// PrepareHost method of the plan type in file. PrepareHost runs real host
// steps and exposes no seam to inspect the steps it assembles, so the
// composition is read from the source.
func hardenAPTCalls(t *testing.T, file, planType string) []string {
	t.Helper()

	parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
	require.NoError(t, err)

	var (
		calls []string
		found bool
	)

	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "PrepareHost" || fn.Recv == nil || len(fn.Recv.List) != 1 {
			continue
		}

		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}

		if ident, ok := recv.(*ast.Ident); !ok || ident.Name != planType {
			continue
		}

		found = true

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}

			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "host" && strings.HasPrefix(sel.Sel.Name, "HardenAPT") {
				calls = append(calls, sel.Sel.Name)
			}

			return true
		})
	}

	require.True(t, found, "%s has no PrepareHost method on %s", file, planType)

	return calls
}

// The nspawn plan hardens APT against machined restarts, which is a step that
// exists only for the machine. The none and container plans have no machine, so
// they must leave it out. Removing the HardenAPTFor wrapper made that a matter
// of which constructor each plan calls.
func TestOnlyTheNSpawnPlanHardensAPT(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"HardenAPT"}, hardenAPTCalls(t, "stageplan_nspawn.go", "nspawnPlan"))
	require.Empty(t, hardenAPTCalls(t, "stageplan_none.go", "nonePlan"), "the none plan has no machined to harden against")
	require.Empty(t, hardenAPTCalls(t, "stageplan_container.go", "containerPlan"), "the container plan has no machined to harden against")
}
