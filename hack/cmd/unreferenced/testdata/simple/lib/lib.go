// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package lib is a fixture for hack/cmd/unreferenced. Every declaration in it
// is named after what the tool is expected to say about it.
package lib

// Widget is referenced by app, so the type itself is live.
type Widget struct {
	Name string
}

// Referenced is called from app.
func (w Widget) Referenced() string { return w.Name }

// Orphan has no caller anywhere. It is the case the tool exists for: a dead
// exported method on a type that is boxed into an interface below, which is
// exactly what makes deadcode's RTA call it reachable-by-reflection.
func (w Widget) Orphan() string { return w.Name }

// String is never named by any identifier, but deleting it would stop Widget
// satisfying fmt.Stringer. The interface filter has to keep it.
func (w Widget) String() string { return w.Name }

// LonelyType is referenced only by its own method's receiver, which does not
// count as a use.
type LonelyType struct{}

// Method is the only thing that mentions LonelyType, and nothing mentions
// Method.
func (LonelyType) Method() string { return "" }

// Used is called from app.
func Used() *Widget { return &Widget{Name: "w"} }

// Unused has no caller.
func Unused() {}

// Recursive calls only itself. Self-reference must not count as a use.
func Recursive(n int) int {
	if n == 0 {
		return 0
	}

	return Recursive(n - 1)
}

// TestedOnly is called from lib_test.go and nowhere else. A reference from a
// test is still a reference, so this must not be reported.
func TestedOnly() string { return "t" }

const (
	// UsedConst is read by app.
	UsedConst = "used"

	// UnusedConst is in the same const group as UsedConst, which is what hides
	// it from staticcheck's unused.
	UnusedConst = "unused"
)

// UnusedVar has no reader.
var UnusedVar = "unused"

// UnusedType has no referent and no methods.
type UnusedType struct{}
