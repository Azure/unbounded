// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package component

import (
	"context"
	"testing"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// fakeCluster is a minimal ClusterComponent for registry tests.
type fakeCluster struct {
	name      string
	condition string
}

func (f fakeCluster) Name() string          { return f.name }
func (f fakeCluster) ConditionType() string { return f.condition }
func (fakeCluster) Plan(context.Context, *Env, []unboundedv1alpha3.Site) (*Plan, Result, error) {
	return NewPlan(), Reconciled(), nil
}

// fakeSite is a minimal SiteComponent for registry tests.
type fakeSite struct {
	name      string
	condition string
}

func (f fakeSite) Name() string                       { return f.name }
func (f fakeSite) ConditionType() string              { return f.condition }
func (fakeSite) Enabled(*unboundedv1alpha3.Site) bool { return true }
func (fakeSite) Plan(context.Context, *Env, *unboundedv1alpha3.Site) (*Plan, Result, error) {
	return NewPlan(), Reconciled(), nil
}

func (fakeSite) CleanupPlan(context.Context, *Env, *unboundedv1alpha3.Site) (*Plan, Result, error) {
	return NewPlan(), Disabled("component disabled"), nil
}

func TestRegistryValidate(t *testing.T) {
	cases := []struct {
		name     string
		registry Registry
		wantErr  bool
	}{
		{
			name:    "empty",
			wantErr: true,
		},
		{
			name: "valid",
			registry: Registry{
				Cluster: []ClusterComponent{fakeCluster{name: "net", condition: "NetReady"}},
				Site:    []SiteComponent{fakeSite{name: "storage", condition: "StorageReady"}},
			},
		},
		{
			name: "duplicate name across lists",
			registry: Registry{
				Cluster: []ClusterComponent{fakeCluster{name: "dup", condition: "AReady"}},
				Site:    []SiteComponent{fakeSite{name: "dup", condition: "BReady"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate condition type",
			registry: Registry{
				Cluster: []ClusterComponent{fakeCluster{name: "a", condition: "SameReady"}},
				Site:    []SiteComponent{fakeSite{name: "b", condition: "SameReady"}},
			},
			wantErr: true,
		},
		{
			name: "empty name",
			registry: Registry{
				Cluster: []ClusterComponent{fakeCluster{name: "", condition: "AReady"}},
			},
			wantErr: true,
		},
		{
			name: "empty condition",
			registry: Registry{
				Site: []SiteComponent{fakeSite{name: "a", condition: ""}},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.registry.Validate()
			if tc.wantErr != (err != nil) {
				t.Fatalf("Validate() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
