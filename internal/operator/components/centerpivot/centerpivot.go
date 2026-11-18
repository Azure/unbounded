// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package centerpivot implements the centerpivot cluster component, which
// updates Azure Container Linux nodes from Nebraska. See
// designs/centerpivot.md.
package centerpivot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	centerpivotmanifests "github.com/Azure/unbounded/deploy/centerpivot"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	name                 = "centerpivot"
	nodeInfoName         = "centerpivot-node-info"
	nodePolicyName       = "centerpivot-node-restriction"
	configHashAnnotation = "unbounded-cloud.io/centerpivot-config-hash"

	// stateAnnotation is the Machine annotation centerpivot keeps each node's
	// state in (controller.StateAnnotation in internal/centerpivot).
	stateAnnotation = "centerpivot.unbounded-cloud.io/state"

	// ReasonUpdatesHeld reports that no update starts while a node is held.
	ReasonUpdatesHeld = "UpdatesHeld"
)

// Component reconciles the centerpivot cluster singleton.
type Component struct{}

// New returns the centerpivot cluster component.
func New() component.ClusterComponent { return Component{} }

func (Component) Name() string          { return name }
func (Component) ConditionType() string { return "CenterPivotReady" }

// EnabledFor reports whether the Site enables centerpivot, with the Nebraska
// URL and apps it needs. It is off unless enabled is true.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	spec := site.Spec.Components.CenterPivot

	return spec != nil && unboundedv1alpha3.ComponentEnabled(&spec.SiteComponentSpec) && spec.NebraskaURL != "" && len(spec.Apps) > 0
}

// Plan applies the singleton while any Site enables it, and removes every
// resource it owns otherwise.
func (c Component) Plan(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	var sources [][2]string

	for i := range sites {
		if !EnabledFor(&sites[i]) {
			continue
		}

		spec := sites[i].Spec.Components.CenterPivot
		for _, app := range spec.Apps {
			source, err := allowedSource(spec.NebraskaURL, app.AppID)
			if err != nil {
				return nil, component.Result{}, err
			}

			if !slices.Contains(sources, source) {
				sources = append(sources, source)
			}
		}
	}

	if len(sources) == 0 {
		return cleanupPlan(env.Namespace), component.Disabled("no Site enables centerpivot"), nil
	}

	slices.SortFunc(sources, func(a, b [2]string) int { return strings.Compare(a[0]+a[1], b[0]+b[1]) })

	plan, err := c.installPlan(env, sources)
	if err != nil {
		return nil, component.Result{}, err
	}

	result, err := heldResult(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	return plan, result, nil
}

func (c Component) installPlan(env *component.Env, sources [][2]string) (*component.Plan, error) {
	var configHash string

	image := env.Config.Image(name)

	objects, err := env.DecodeManifestFS(centerpivotmanifests.Manifests, func(obj *unstructured.Unstructured) error {
		switch {
		case obj.GetKind() == "ConfigMap" && obj.GetName() == name:
			data, _, err := unstructured.NestedMap(obj.Object, "data")
			if err != nil {
				return fmt.Errorf("read centerpivot config: %w", err)
			}

			encoded, err := json.Marshal(data)
			if err != nil {
				return fmt.Errorf("encode centerpivot config: %w", err)
			}

			sum := sha256.Sum256(encoded)
			configHash = hex.EncodeToString(sum[:])
		case obj.GetKind() == "Deployment" && obj.GetName() == name:
			if configHash == "" {
				return fmt.Errorf("centerpivot Deployment decoded before its ConfigMap")
			}

			if err := component.SetPodSpecImages(obj, image); err != nil {
				return fmt.Errorf("set centerpivot image: %w", err)
			}

			if err := unstructured.SetNestedField(obj.Object, configHash,
				"spec", "template", "metadata", "annotations", configHashAnnotation); err != nil {
				return fmt.Errorf("set centerpivot config hash: %w", err)
			}
		case obj.GetKind() == "DaemonSet" && obj.GetName() == nodeInfoName:
			if err := component.SetPodSpecImages(obj, image); err != nil {
				return fmt.Errorf("set centerpivot-node-info image: %w", err)
			}
		case obj.GetKind() == "ValidatingAdmissionPolicy" && obj.GetName() == nodePolicyName:
			variables := []any{map[string]any{"name": "allowedSources", "expression": sourcesExpression(sources)}}
			if err := unstructured.SetNestedSlice(obj.Object, variables, "spec", "variables"); err != nil {
				return fmt.Errorf("set the allowed Nebraska sources: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	plan := component.NewPlan()

	for _, obj := range objects {
		op := component.Operation{Kind: component.OpApply, Object: obj, Component: c.Name()}
		if obj.GetKind() == "Deployment" || obj.GetKind() == "DaemonSet" {
			op.Overridable = true
		}

		plan.Add(op)
	}

	return plan, nil
}

// allowedSource returns the JSON fragments that name a Site's Nebraska URL
// and one of its app IDs in an update request, written as the service writes
// them.
func allowedSource(nebraskaURL, id string) ([2]string, error) {
	server, err := json.Marshal(nebraskaURL)
	if err != nil {
		return [2]string{}, fmt.Errorf("encode Nebraska URL: %w", err)
	}

	appID, err := json.Marshal(id)
	if err != nil {
		return [2]string{}, fmt.Errorf("encode app ID: %w", err)
	}

	return [2]string{`"server":` + string(server), `"appId":` + string(appID)}, nil
}

// sourcesExpression returns the sources as a CEL list of string pairs.
func sourcesExpression(sources [][2]string) string {
	pairs := make([]string, 0, len(sources))
	for _, source := range sources {
		pairs = append(pairs, "["+strconv.Quote(source[0])+", "+strconv.Quote(source[1])+"]")
	}

	return "[" + strings.Join(pairs, ", ") + "]"
}

// heldResult reports any node held after a failed update: until a person
// clears it, no update starts.
func heldResult(ctx context.Context, env *component.Env) (component.Result, error) {
	machines := &metav1.PartialObjectMetadataList{}
	machines.SetGroupVersionKind(unboundedv1alpha3.GroupVersion.WithKind("MachineList"))

	if err := env.Client.List(ctx, machines); err != nil {
		return component.Result{}, fmt.Errorf("list Machines: %w", err)
	}

	var held []string

	reasons := map[string]string{}

	for _, machine := range machines.Items {
		if reason, ok := heldReason(machine.Annotations); ok {
			held = append(held, machine.Name)
			reasons[machine.Name] = reason
		}
	}

	if len(held) == 0 {
		return component.Reconciled(), nil
	}

	slices.Sort(held)

	return component.NotReady(ReasonUpdatesHeld, fmt.Sprintf(
		"no update starts while Machine %s is held (%d held): %s. Remove its %s annotation to clear the hold",
		held[0], len(held), reasons[held[0]], stateAnnotation,
	)), nil
}

// heldReason returns why a Machine's node is held, if it is.
func heldReason(annotations map[string]string) (string, bool) {
	value, ok := annotations[stateAnnotation]
	if !ok {
		return "", false
	}

	var state struct {
		Phase  string `json:"phase"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(value), &state); err != nil {
		return "its state can't be read", true
	}

	return state.Reason, state.Phase == "Held"
}

func cleanupPlan(namespace string) *component.Plan {
	plan := component.NewPlan()

	for _, obj := range []struct {
		kind, api, name, namespace string
	}{
		{kind: "Deployment", api: "apps/v1", name: name, namespace: namespace},
		{kind: "DaemonSet", api: "apps/v1", name: nodeInfoName, namespace: namespace},
		{kind: "ConfigMap", api: "v1", name: name, namespace: namespace},
		{kind: "ValidatingAdmissionPolicyBinding", api: "admissionregistration.k8s.io/v1", name: nodePolicyName},
		{kind: "ValidatingAdmissionPolicy", api: "admissionregistration.k8s.io/v1", name: nodePolicyName},
		{kind: "ValidatingAdmissionPolicyBinding", api: "admissionregistration.k8s.io/v1", name: "centerpivot-machine-restriction"},
		{kind: "ValidatingAdmissionPolicy", api: "admissionregistration.k8s.io/v1", name: "centerpivot-machine-restriction"},
		{kind: "ValidatingAdmissionPolicyBinding", api: "admissionregistration.k8s.io/v1", name: "centerpivot-node-info-restriction"},
		{kind: "ValidatingAdmissionPolicy", api: "admissionregistration.k8s.io/v1", name: "centerpivot-node-info-restriction"},
		{kind: "RoleBinding", api: "rbac.authorization.k8s.io/v1", name: name, namespace: namespace},
		{kind: "Role", api: "rbac.authorization.k8s.io/v1", name: name, namespace: namespace},
		{kind: "ClusterRoleBinding", api: "rbac.authorization.k8s.io/v1", name: name},
		{kind: "ClusterRole", api: "rbac.authorization.k8s.io/v1", name: name},
		{kind: "ClusterRoleBinding", api: "rbac.authorization.k8s.io/v1", name: nodeInfoName},
		{kind: "ClusterRole", api: "rbac.authorization.k8s.io/v1", name: nodeInfoName},
		{kind: "ServiceAccount", api: "v1", name: name, namespace: namespace},
		{kind: "ServiceAccount", api: "v1", name: nodeInfoName, namespace: namespace},
	} {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(obj.api)
		u.SetKind(obj.kind)
		u.SetName(obj.name)
		u.SetNamespace(obj.namespace)
		plan.Add(component.Operation{Kind: component.OpDelete, Object: u, Component: name})
	}

	return plan
}

// SetupWatches reconciles drift and deletion of the managed workloads and
// config, and a node becoming held or no longer held.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(name))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(nodeInfoName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(name))))
	b.Watches(&unboundedv1alpha3.Machine{}, env.RequestSingleton(),
		builder.WithPredicates(heldChanged()), builder.OnlyMetadata)
}

// heldChanged passes Machine events that change whether, or why, a node is
// held.
func heldChanged() predicate.Predicate {
	held := func(obj client.Object) string {
		reason, ok := heldReason(obj.GetAnnotations())
		if !ok {
			return ""
		}

		return "held: " + reason
	}

	return predicate.Funcs{
		CreateFunc:  func(ev event.CreateEvent) bool { return held(ev.Object) != "" },
		DeleteFunc:  func(ev event.DeleteEvent) bool { return held(ev.Object) != "" },
		UpdateFunc:  func(ev event.UpdateEvent) bool { return held(ev.ObjectOld) != held(ev.ObjectNew) },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}
