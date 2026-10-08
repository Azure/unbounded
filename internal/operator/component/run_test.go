// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package component

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestExecuteRunSuccess(t *testing.T) {
	env, calls := recordingEnv(t)
	ctx := t.Context()
	count := 0
	op := Operation{
		Kind: OpRun, Object: configMapObject("action"), Component: "security", Site: "site-a",
		Run: func(got context.Context) error {
			if got != ctx {
				t.Fatal("callback did not receive execution context")
			}

			count++

			return nil
		},
	}
	before := op.Object.DeepCopy()
	plan := NewPlan()
	plan.Add(op)

	if !strings.HasPrefix(plan.Summary(), "Run ") {
		t.Fatalf("summary = %q", plan.Summary())
	}

	if _, err := plan.ExecutionOrder(); err != nil {
		t.Fatal(err)
	}

	if count != 0 {
		t.Fatal("planning ran callback")
	}

	result, err := env.Execute(ctx, plan)
	if err != nil || result.Err() != nil {
		t.Fatalf("Execute: %v; results: %v", err, result.Err())
	}

	if count != 1 || len(*calls) != 0 || !reflect.DeepEqual(before, op.Object) {
		t.Fatalf("count = %d, writes = %v, object = %#v", count, *calls, op.Object)
	}

	want := OperationResult{Ref: op.Ref(), Kind: OpRun, Component: "security", Site: "site-a", Status: OpSucceeded}
	if len(result.Results) != 1 || result.Results[0] != want {
		t.Fatalf("results = %#v, want %#v", result.Results, want)
	}
}

func TestExecuteRunDependencies(t *testing.T) {
	sentinel := errors.New("security initialization failed")
	for _, tc := range []struct {
		name      string
		err       error
		status    OpStatus
		dependent OpStatus
	}{
		{name: "success", status: OpSucceeded, dependent: OpSucceeded},
		{name: "failure", err: sentinel, status: OpFailed, dependent: OpSkipped},
		{name: "conflict", err: apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "action", sentinel), status: OpDeferred, dependent: OpDeferred},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, calls := recordingEnv(t)
			dependency := configMapOp("dependency")
			action := Operation{
				Kind: OpRun, Object: configMapObject("action"), Component: "test", Site: "site-a",
				DependsOn: []ObjectRef{dependency.Ref()},
				Run: func(context.Context) error {
					*calls = append(*calls, "run action")

					return tc.err
				},
			}
			dependent := configMapOp("dependent")
			dependent.DependsOn = []ObjectRef{action.Ref()}
			independent := configMapOp("independent")
			independent.Component = "other"
			plan := NewPlan()
			plan.Add(dependent, action, dependency, independent)

			result, err := env.Execute(t.Context(), plan)
			if err != nil {
				t.Fatal(err)
			}

			wantCalls := []string{"apply ConfigMap/dependency", "run action"}
			if tc.dependent == OpSucceeded {
				wantCalls = append(wantCalls, "apply ConfigMap/dependent")
			}

			wantCalls = append(wantCalls, "apply ConfigMap/independent")
			assertCalls(t, *calls, wantCalls)

			if len(result.Results) != 4 || result.Results[1].Status != tc.status || result.Results[2].Status != tc.dependent {
				t.Fatalf("results = %#v", result.Results)
			}

			if tc.status == OpFailed && !errors.Is(result.Err(), sentinel) {
				t.Fatalf("lost callback error: %v", result.Err())
			}

			if tc.status != OpFailed && result.Err() != nil {
				t.Fatalf("unexpected failure: %v", result.Err())
			}
		})
	}
}

func TestExecuteRunCanceled(t *testing.T) {
	env, calls := recordingEnv(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	plan := NewPlan()
	plan.Add(Operation{Kind: OpRun, Object: configMapObject("action"), Run: func(context.Context) error {
		t.Fatal("canceled callback ran")

		return nil
	}})

	result, err := env.Execute(ctx, plan)
	if err != nil || !errors.Is(result.Err(), context.Canceled) || len(result.Failed()) != 1 || len(*calls) != 0 {
		t.Fatalf("Execute = %#v, %v; writes = %v", result, err, *calls)
	}
}

func TestExecuteRunBlockedDependency(t *testing.T) {
	env, calls := recordingEnv(t)
	dependency := Operation{Kind: OpMergePatch, Object: configMapObject("dependency")}
	plan := NewPlan()
	plan.Add(Operation{
		Kind: OpRun, Object: configMapObject("action"), DependsOn: []ObjectRef{dependency.Ref()},
		Run: func(context.Context) error {
			t.Fatal("callback ran after failed dependency")

			return nil
		},
	}, dependency)

	result, err := env.Execute(t.Context(), plan)
	if err != nil || len(result.Failed()) != 1 || len(result.Skipped()) != 1 || len(*calls) != 0 {
		t.Fatalf("Execute = %#v, %v; writes = %v", result, err, *calls)
	}
}

func TestExecuteRunPreflight(t *testing.T) {
	for _, name := range []string{"nil callback", "shared", "overridable", "nil object", "missing kind", "missing version", "apply callback", "create callback", "patch callback", "delete callback", "cycle"} {
		t.Run(name, func(t *testing.T) {
			env, calls := recordingEnv(t)
			run := func(context.Context) error {
				t.Fatal("callback ran before preflight completed")

				return nil
			}
			first := Operation{Kind: OpRun, Object: configMapObject("first"), Run: run}
			invalid := Operation{Kind: OpRun, Object: configMapObject("invalid"), Run: run}

			switch name {
			case "nil callback":
				invalid.Run = nil
			case "shared":
				invalid.SharedKey = "shared"
			case "overridable":
				invalid.Overridable = true
			case "nil object":
				invalid.Object = nil
			case "missing kind":
				invalid.Object.SetKind("")
			case "missing version":
				invalid.Object.SetAPIVersion("")
			case "apply callback":
				invalid.Kind = OpApply
			case "create callback":
				invalid.Kind = OpCreateIfAbsent
			case "patch callback":
				invalid.Kind = OpMergePatch
			case "delete callback":
				invalid.Kind = OpDelete
			case "cycle":
				first.DependsOn = []ObjectRef{invalid.Ref()}
				invalid.DependsOn = []ObjectRef{first.Ref()}
			}

			plan := NewPlan()
			plan.Add(configMapOp("ordinary"), first, invalid)

			result, err := env.Execute(t.Context(), plan)
			if err == nil || len(result.Results) != 0 || len(*calls) != 0 {
				t.Fatalf("Execute = %#v, %v; writes = %v", result, err, *calls)
			}
		})
	}
}
