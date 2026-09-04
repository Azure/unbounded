// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantry implements the gantry peer-to-peer OCI distribution cluster
// component. Gantry runs one agent DaemonSet per node fronting containerd's
// image pulls, so it is reconciled as a cluster singleton. Unlike the other
// components it defaults to enabled: it is deployed unless every Site explicitly
// opts out via spec.components.gantry.enabled=false.
//
// The unbounded agent manages the per-node containerd wiring, so the operator
// installs only the Gantry workload and its Kubernetes configuration.
package gantry

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	daemonSetName = "gantry"
	configName    = "gantry-config"

	// imageRepository is the operator-managed image repository for the gantry
	// agent. The operator derives the full reference at reconcile time via
	// component.Config.Image so gantry is version-matched to the operator like
	// the other components.
	imageRepository = "gantry"

	// agentContainerName is the gantry agent's main container. Only this
	// container carries the operator-managed image; the DaemonSet's busybox
	// init container keeps its pinned public image.
	agentContainerName = "gantry"

	// legacyNodeConfigName and legacyNodeConfigDaemonSetName were installed by
	// older operator versions. The unbounded agent owns this host configuration.
	legacyNodeConfigName          = "gantry-containerd-hosts"
	legacyNodeConfigDaemonSetName = "gantry-containerd-config"

	configHashAnnotation = "unbounded-cloud.io/gantry-config-hash"
)

// Component reconciles the gantry cluster singleton.
type Component struct{}

// New returns the gantry cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "gantry" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "GantryReady" }

// EnabledFor reports whether a Site enables the gantry component. Gantry
// defaults to enabled: only an explicit enabled=false opts a Site out.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	g := site.Spec.Components.Gantry
	if g == nil || g.Enabled == nil {
		return true
	}

	return *g.Enabled
}

// Reconcile deploys the gantry cluster singleton whenever at least one Site does
// not opt out and keeps an existing installation reconciled when every Site opts
// out.
func (c Component) Plan(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	plan := component.NewPlan()

	// The legacy node config is removed before anything else is applied, and
	// the applies depend on those deletes, so a failure to remove the legacy
	// DaemonSet does not race the replacement into the cluster alongside it.
	legacy := legacyCleanupOperations(c.Name(), env.Namespace)
	plan.Add(legacy...)

	legacyRefs := make([]component.ObjectRef, 0, len(legacy))
	for _, op := range legacy {
		legacyRefs = append(legacyRefs, op.Ref())
	}

	enabled := false

	for i := range sites {
		if EnabledFor(&sites[i]) {
			enabled = true
			break
		}
	}

	if !enabled {
		// Keep gantry installed once the operator has taken ownership; automatic
		// singleton removal is surprising. A future explicit uninstall flow
		// should handle removal.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return nil, component.Result{}, err
		}

		if !retained {
			return plan, component.Disabled("no site enables gantry"), nil
		}
	}

	configHash, configOp, err := planConfig(ctx, env)
	if err != nil {
		return nil, component.Result{}, err
	}

	dependsOn := legacyRefs

	if configOp != nil {
		plan.Add(*configOp)

		dependsOn = append(dependsOn, configOp.Ref())
	}

	objects, err := decodeManifests(env, applyMutator(env.Config.Image(imageRepository), configHash))
	if err != nil {
		return nil, component.Result{}, err
	}

	for _, obj := range objects {
		kind := component.OpApply
		if obj.GetKind() == "Lease" && strings.HasPrefix(obj.GetName(), "gantry-chair-") {
			kind = component.OpCreateIfAbsent
		}

		op := component.Operation{
			Kind:      kind,
			Object:    obj,
			Component: c.Name(),
			DependsOn: dependsOn,
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName {
			op.Overridable = true
		}

		plan.Add(op)
	}

	return plan, component.Reconciled(), nil
}

// SetupWatches reconciles Gantry on changes to its active resources and when
// legacy node-config resources appear so they can be removed.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	// The singleton request already fans out to every Site, so enqueuing
	// the Sites as well would run one redundant pass per Site for a single
	// ConfigMap edit.
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName, legacyNodeConfigName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(daemonSetName, legacyNodeConfigDaemonSetName))))
	b.Watches(&coordinationv1.Lease{}, env.RequestSingleton(),
		builder.WithPredicates(chairLeaseDeletePredicate(env.Namespace)))
}

func chairLeaseDeletePredicate(namespace string) predicate.Predicate {
	match := func(obj client.Object) bool {
		if obj.GetNamespace() != namespace || !strings.HasPrefix(obj.GetName(), "gantry-chair-") {
			return false
		}

		suffix := strings.TrimPrefix(obj.GetName(), "gantry-chair-")
		index, err := strconv.Atoi(suffix)

		return err == nil && len(suffix) == 2 && index >= 0 && index < 64
	}

	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return false },
		DeleteFunc:  func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc:  func(event.UpdateEvent) bool { return false },
		GenericFunc: func(event.GenericEvent) bool { return false },
	}
}

// decodeManifests decodes Gantry's operator-managed top-level manifests. The
// standalone node configurator and examples subtree are intentionally excluded.
func decodeManifests(env *component.Env, mutate func(*unstructured.Unstructured) error) ([]*unstructured.Unstructured, error) {
	files, err := component.YamlFiles(gantrymanifests.Manifests)
	if err != nil {
		return nil, err
	}

	topLevel := make([]string, 0, len(files))

	for _, file := range files {
		if file == "node-config.yaml" || strings.Contains(file, "/") {
			continue
		}

		topLevel = append(topLevel, file)
	}

	return env.DecodeManifestFiles(gantrymanifests.Manifests, topLevel, mutate)
}

func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, resource := range []struct {
		name   string
		object client.Object
	}{
		{name: configName, object: &corev1.ConfigMap{}},
		{name: daemonSetName, object: &appsv1.DaemonSet{}},
	} {
		key := client.ObjectKey{Namespace: env.Namespace, Name: resource.name}
		if err := env.Client.Get(ctx, key, resource.object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained gantry resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}

func legacyCleanupOperations(componentName, namespace string) []component.Operation {
	return []component.Operation{
		component.DeleteOperation(&appsv1.DaemonSet{
			TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: legacyNodeConfigDaemonSetName},
		}, componentName, ""),
		component.DeleteOperation(&corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: legacyNodeConfigName},
		}, componentName, ""),
	}
}

// applyMutator skips CRDs and the separately reconciled gantry-config ConfigMap,
// stamps the operator-derived agent image onto the agent DaemonSet, and stamps
// the config payload hash on its pod template so a config change rolls the
// DaemonSet. Only the agent's own container is re-imaged.
func applyMutator(agentImage, configHash string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "ConfigMap" && obj.GetName() == configName {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() == "DaemonSet" && obj.GetName() == daemonSetName {
			if err := component.SetNamedContainerImage(obj, agentContainerName, agentImage); err != nil {
				return fmt.Errorf("set gantry agent image: %w", err)
			}

			return stampConfigHash(obj, configHashAnnotation, configHash)
		}

		return nil
	}
}

// stampConfigHash sets a config-hash annotation on a workload's pod template so a
// config change rolls it.
func stampConfigHash(obj *unstructured.Unstructured, annotation, hash string) error {
	annotations, _, err := unstructured.NestedStringMap(obj.Object, "spec", "template", "metadata", "annotations")
	if err != nil {
		return fmt.Errorf("get %s pod template annotations: %w", obj.GetName(), err)
	}

	if annotations == nil {
		annotations = map[string]string{}
	}

	annotations[annotation] = hash
	if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
		return fmt.Errorf("set %s config hash annotation: %w", obj.GetName(), err)
	}

	return nil
}

// planConfig hashes the gantry config payload and, when no config exists,
// returns the operation that creates the embedded default. An existing
// operator or user-managed payload (for example an edited upstream_registries
// list) is never applied over, so the returned operation is create-if-absent.
//
// See net.planConfig for why the hash is computed at plan time and what happens
// if another writer wins the create.
func planConfig(ctx context.Context, env *component.Env) (string, *component.Operation, error) {
	key := client.ObjectKey{Namespace: env.Namespace, Name: configName}
	existing := &corev1.ConfigMap{}

	err := env.Client.Get(ctx, key, existing)
	if err == nil {
		return component.ConfigMapPayloadHash(existing), nil, nil
	}

	if !apierrors.IsNotFound(err) {
		return "", nil, fmt.Errorf("get gantry config %s/%s: %w", key.Namespace, key.Name, err)
	}

	desired, err := env.DefaultConfigMap(gantrymanifests.Manifests, configName, "gantry")
	if err != nil {
		return "", nil, err
	}

	return component.ConfigMapPayloadHash(desired), &component.Operation{
		Kind:      component.OpCreateIfAbsent,
		Object:    component.ToUnstructured(desired),
		Component: Component{}.Name(),
	}, nil
}
