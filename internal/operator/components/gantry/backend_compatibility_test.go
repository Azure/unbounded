// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer/workload"
)

func TestPlanRejectsArtifactStreamingWithRacerCache(t *testing.T) {
	for _, test := range []struct {
		name      string
		dataplane bool
	}{
		{name: "pending dataplane"},
		{name: "ready dataplane", dataplane: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			objects := []client.Object{racerCache()}
			if test.dataplane {
				objects = append(objects, racerDataplane())
			}

			enabled := true
			site := siteWithGantry("edge", &enabled)
			site.Spec.Components.Gantry.ArtifactStreaming = &unboundedv1alpha3.GantryArtifactStreamingSpec{
				Enabled: true, NodeSelector: map[string]string{"pool": "streaming"},
			}

			plan, _, err := (Component{}).Plan(t.Context(), testEnv(t, objects...), []unboundedv1alpha3.Site{*site})
			if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
				t.Fatalf("Plan error = %v, want explicit unsupported-backend error", err)
			}

			if plan != nil {
				t.Fatal("unsupported backend combination returned a deployment plan")
			}
		})
	}
}

func TestApplyMutatorRejectsArtifactStreamingWithRacer(t *testing.T) {
	object := &unstructured.Unstructured{Object: map[string]any{
		"kind": "DaemonSet", "metadata": map[string]any{"name": daemonSetName},
	}}
	mutate := applyMutator("gantry:test", "gantry-node-config:test", "hash",
		artifactStreamingConfig{Enabled: true}, &workload.NodePlacement{})

	err := mutate(object)
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("mutator error = %v, want explicit unsupported-backend error", err)
	}
}
