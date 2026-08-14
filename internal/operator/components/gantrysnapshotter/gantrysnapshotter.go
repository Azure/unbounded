// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gantrysnapshotter implements the gantry-snapshotter cluster
// component: a containerd snapshotter that reads container image layers out of
// RACER instead of pulling and unpacking them per node.
//
// The component installs two DaemonSets and provisions the volume they read.
// The volume is the interesting half. A racer volume's geometry is frozen at
// creation and racer has no controller service, so nothing in the cluster
// creates PersistentVolumes on demand; the racer allocator only assigns extents
// to volumes that already exist. This component is therefore where the image
// volume comes from, and it creates it statically: one volume whose composition
// is a small OCC catalog extent, mapping a containerd chain ID to a byte range,
// followed by the IMMUTABLE_4M extents the layer bytes live in.
//
// The division of labour with the racer component is deliberate and worth
// stating: this component only ever creates that PersistentVolume, and the
// racer component only ever annotates it. Two writers of the same racer
// annotations would race over extent allocation, which is not recoverable
// because an allocated extent is a byte range other volumes are then placed
// after.
package gantrysnapshotter

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	snapshottermanifests "github.com/Azure/unbounded/deploy/gantry-snapshotter"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	// daemonSetName is the snapshotter agent itself.
	daemonSetName = "gantry-snapshotter"

	// nodeConfigDaemonSetName is the agent that owns the node's containerd
	// configuration.
	nodeConfigDaemonSetName = "gantry-snapshotter-node-config"

	// imageRepository is the operator-managed image repository. Both DaemonSets
	// run the same binary, so both carry this image.
	imageRepository = "gantry-snapshotter"

	// containerName is the snapshotter's container in both DaemonSets. They are
	// named differently on purpose, so re-imaging is done by pod spec rather
	// than by name.
	containerName     = "gantry-snapshotter"
	nodeConfigCtrName = "node-config"

	// requeueInterval re-checks the image volumes while they are waiting for
	// the racer allocator to place them. Nothing here watches the racer
	// annotations, and the wait is short.
	requeueInterval = 15 * time.Second
)

// Component reconciles the gantry-snapshotter cluster singleton.
type Component struct{}

// New returns the gantry-snapshotter cluster component.
func New() component.ClusterComponent { return Component{} }

// Name implements component.ClusterComponent.
func (Component) Name() string { return "gantry-snapshotter" }

// ConditionType implements component.ClusterComponent.
func (Component) ConditionType() string { return "GantrySnapshotterReady" }

// EnabledFor reports whether a Site enables the gantry-snapshotter component.
//
// It defaults to disabled, unlike the gantry mirror. Enabling it reconfigures
// containerd on every racer node so that container creation depends on a
// socket, which is not something to opt a cluster into by omission.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	spec := site.Spec.Components.GantrySnapshotter
	if spec == nil || spec.Enabled == nil {
		return false
	}

	return *spec.Enabled
}

// Reconcile installs the snapshotter once a Site enables it, and keeps an
// existing installation reconciled if every Site later opts out.
func (Component) Reconcile(ctx context.Context, env *component.Env, sites []unboundedv1alpha3.Site) component.Result {
	var enabling *unboundedv1alpha3.Site

	for i := range sites {
		if EnabledFor(&sites[i]) {
			enabling = &sites[i]

			break
		}
	}

	if enabling == nil {
		// Uninstalling would leave nodes whose containerd still points at a
		// snapshotter that is no longer scheduled there, which is how a node
		// stops being able to start pods at all. Removal has to be deliberate.
		retained, err := resourcesExist(ctx, env)
		if err != nil {
			return component.Failed(err)
		}

		if !retained {
			return component.Disabled("no site enables gantry-snapshotter")
		}
	}

	layout, err := layoutFor(enabling)
	if err != nil {
		return component.Failed(err)
	}

	pending, err := ensureImageVolume(ctx, env, layout)
	if err != nil {
		return component.Failed(err)
	}

	if err := applyManifests(ctx, env, applyMutator(env.Config.Image(imageRepository))); err != nil {
		return component.Failed(err)
	}

	if pending != "" {
		return component.NotReadyAfter("ImageVolumePending", pending, requeueInterval)
	}

	return component.Reconciled()
}

// SetupWatches reconciles on changes to the workloads and to the image volumes,
// so that a PersistentVolume deleted by hand is recreated.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(
			env.InNamespaceNamed(daemonSetName, nodeConfigDaemonSetName))))
	b.Watches(&corev1.PersistentVolume{}, env.RequestSingleton())
}

func applyManifests(ctx context.Context, env *component.Env, mutate func(*unstructured.Unstructured) error) error {
	return env.ApplyManifestFS(ctx, snapshottermanifests.Manifests, mutate)
}

// applyMutator stamps the operator-derived image onto both DaemonSets.
func applyMutator(image string) func(*unstructured.Unstructured) error {
	return func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == component.CRDKind {
			obj.Object = nil

			return nil
		}

		if obj.GetKind() != "DaemonSet" {
			return nil
		}

		name := containerName
		if obj.GetName() == nodeConfigDaemonSetName {
			name = nodeConfigCtrName
		}

		if err := component.SetNamedContainerImage(obj, name, image); err != nil {
			return fmt.Errorf("set %s image: %w", obj.GetName(), err)
		}

		return nil
	}
}

func resourcesExist(ctx context.Context, env *component.Env) (bool, error) {
	for _, name := range []string{daemonSetName, nodeConfigDaemonSetName} {
		key := client.ObjectKey{Namespace: env.Namespace, Name: name}
		if err := env.Client.Get(ctx, key, &appsv1.DaemonSet{}); err == nil {
			return true, nil
		} else if !apierrors.IsNotFound(err) {
			return false, fmt.Errorf("get retained gantry-snapshotter resource %s/%s: %w", key.Namespace, key.Name, err)
		}
	}

	return false, nil
}
