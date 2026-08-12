// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package racer implements the racer distributed block device cluster
// component.
//
// racer's dataplane is deliberately ignorant: it reads one config file and
// exports what that file describes. Everything that has to be decided across
// machines - who is in a replication group, which extent lives where, what id a
// node answers to - has to be decided somewhere else, exactly once, by one
// writer. This component is that writer.
//
// It owns five pieces of cluster state and nothing else owns any of them:
//
//   - the racer-allocations ConfigMap, holding the never-reused id cursors and
//     the zone definitions automatic placement decides from;
//   - one ConfigMap per universe per zone, holding that zone's catalog
//     membership;
//   - StorageClass annotations, because a StorageClass with racer's provisioner
//     is a universe;
//   - PersistentVolume annotations, because a PV bound to such a class is a
//     volume, and its composition is the extent placement stamped once and
//     frozen;
//   - Node identity annotations (node-id, cohort, zone).
//
// The node agent (racer-ctrl) reads all five and writes only its own node's
// status. Nothing else in the cluster writes any of them. That single-writer
// property is what makes the allocation safe without a lease of its own: the
// operator is already leader-elected, so there is exactly one of it.
//
// The R6 sequences (membership replacement, extent migration, tombstone
// collection, decommission) run here too. They are gated on what the nodes
// publish about the dataplane, never on a timer, and the gates live in
// internal/racerctrl as pure predicates so they can be tested without a cluster.
package racer

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racermanifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racerctrl"
)

const (
	// daemonSetName is the node agent workload. It carries racer-ctrl, the
	// racer dataplane, and the kubelet plugin registrar in one pod so the three
	// share the config directory and the node's lifetime.
	daemonSetName = "racer"

	// ctrlContainerName and racerContainerName are the two containers that
	// carry operator-derived images. The registrar keeps its pinned upstream
	// image.
	ctrlContainerName  = "racer-ctrl"
	racerContainerName = "racer"

	// ctrlImageRepository and racerImageRepository are version-matched to the
	// operator through component.Config.Image, like every other component.
	ctrlImageRepository  = "racer-ctrl"
	racerImageRepository = "racer"

	// EnrollmentLabel opts a node into racer. The DaemonSet selects on it and
	// this component allocates identity only for nodes that carry it, so
	// enrolling a node is a single label and decommissioning it is removing
	// that label.
	EnrollmentLabel = "racer.unbounded-cloud.io/enabled"

	// ZoneLabel is the availability zone a node sits in, the standard
	// Kubernetes topology label. It is not a racer zone: a racer zone is a
	// catalog and spans several availability zones on purpose, because a trio
	// takes one node from each cohort and a cohort is an availability zone.
	// This is the input placement balances a zone's cohorts against.
	ZoneLabel = corev1.LabelTopologyZone

	// requeueInterval is how long to wait before looking again while a sequence
	// is in flight. The sequences are gated on scraped dataplane metrics, which
	// the node agent republishes every 15 seconds, so looking more often than
	// that only re-reads the same numbers.
	requeueInterval = 15 * time.Second
)

// Component reconciles the racer cluster singleton.
type Component struct{}

// New returns the racer cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "racer" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "RacerReady" }

// EnabledFor reports whether a Site enables racer. Racer defaults to disabled:
// it takes over block devices and a store file on every node it runs on, which
// is not something to switch on by omission.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	r := site.Spec.Components.Racer
	if r == nil {
		return false
	}

	return unboundedv1alpha3.ComponentEnabled(&r.SiteComponentSpec)
}

// Reconcile installs the racer node agent and then advances the cluster-wide
// state it owns.
//
// The two halves are deliberately ordered. Installing first means a fresh
// cluster gets its agents running and reporting before the allocator starts
// making decisions that depend on what they report; a gate that has never heard
// from a node blocks, which is the correct answer rather than a stall.
func (Component) Reconcile(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	enabled := false

	for i := range sites {
		if EnabledFor(&sites[i]) {
			enabled = true
			break
		}
	}

	if !enabled {
		// Racer holds data. Uninstalling it because the last Site stopped
		// asking for it would strand every extent on every node, so an existing
		// installation keeps being reconciled until someone removes it
		// deliberately.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return component.Failed(err)
		}

		if !retained {
			return component.Disabled("no site enables racer")
		}
	}

	mutate := applyMutator(env.Config.Image(ctrlImageRepository), env.Config.Image(racerImageRepository))
	if err := env.ApplyManifestFS(ctx, racermanifests.Manifests, mutate); err != nil {
		return component.Failed(err)
	}

	return reconcileState(ctx, env)
}

// SetupWatches reconciles racer on changes to the objects that carry its state.
// Nodes are watched because a node's identity and its published health both
// live in its annotations, and both feed decisions made here.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(daemonSetName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(func(obj client.Object) bool {
			return env.InNamespaceNamed(allocationsName)(obj) ||
				env.InNamespaceWithPrefix(racerctrl.MembershipConfigMapPrefix)(obj)
		})))
	b.Watches(&corev1.Node{}, env.RequestSingleton())
	b.Watches(&corev1.PersistentVolume{}, env.RequestSingleton())
}

// reconcileState runs the allocation and sequencing passes and folds their
// results into one condition.
//
// A blocked gate is not an error. It is the system telling us that the previous
// step has not landed yet, which is exactly what R6 asks us to wait for, so it
// becomes a not-ready condition with a requeue rather than a failure.
func reconcileState(ctx context.Context, env *component.Env) component.Result {
	pass, err := loadState(ctx, env)
	if err != nil {
		return component.Failed(err)
	}

	if err := pass.allocate(ctx); err != nil {
		return component.Failed(err)
	}

	if err := pass.sequence(ctx); err != nil {
		return component.Failed(err)
	}

	if len(pass.waiting) > 0 {
		return component.NotReadyAfter("Sequencing", strings.Join(pass.waiting, "; "), requeueInterval)
	}

	return component.Reconciled()
}

// resourcesExist reports whether racer is still installed, so a disabled
// component can tell "never installed" from "installed and now unwanted".
func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, resource := range []struct {
		name   string
		object client.Object
	}{
		{name: daemonSetName, object: &appsv1.DaemonSet{}},
		{name: allocationsName, object: &corev1.ConfigMap{}},
	} {
		key := client.ObjectKey{Namespace: env.Namespace, Name: resource.name}
		if err := env.Client.Get(ctx, key, resource.object); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained racer resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}

// applyMutator stamps the operator-derived images onto the two containers the
// operator builds and leaves everything else alone.
//
// StorageClasses are dropped rather than applied. A racer StorageClass is a
// universe, and this component writes the universe id, catalog size, epoch and
// LBA cursor onto it as annotations. Server-side applying the same object from a
// manifest would remove the fields the same field manager set by patch, so the
// default class is created once by ensureDefaultClass and never reapplied. The
// case is kept as a guard: a StorageClass that reappears in the manifest set
// should be ignored, not silently allowed to fight the allocator.
func applyMutator(ctrlImage, racerImage string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		switch obj.GetKind() {
		case component.CRDKind, "StorageClass":
			obj.Object = nil

			return nil
		case "DaemonSet":
			if obj.GetName() != daemonSetName {
				return nil
			}

			if err := component.SetNamedContainerImage(obj, ctrlContainerName, ctrlImage); err != nil {
				return fmt.Errorf("set racer-ctrl image: %w", err)
			}

			if err := component.SetNamedContainerImage(obj, racerContainerName, racerImage); err != nil {
				return fmt.Errorf("set racer image: %w", err)
			}

			return nil
		default:
			return nil
		}
	}
}
