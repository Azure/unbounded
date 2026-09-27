// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer provisions Racer on demand from cluster-scoped ClusterCaches.
package racer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
)

const (
	name                   = "racer"
	controllerName         = "racer-controller"
	claimName              = "racer-operator-installation"
	markerName             = "racer-installation"
	versionName            = "racer-version"
	configName             = "racer-config"
	jobName                = "racer-initialize"
	tlsName                = "racer-controller-tls"
	trustName              = "racer-bootstrap-trust"
	managerAnnotation      = "racer.unbounded-cloud.io/manager"
	claimAnnotation        = "racer.unbounded-cloud.io/operator-claim-uid"
	installationAnnotation = "racer.unbounded-cloud.io/installation-uid"
)

// Component owns provisioning, while the Racer controller owns the dataplane.
type Component struct{}

func New() component.ClusterComponent   { return Component{} }
func (Component) Name() string          { return name }
func (Component) ConditionType() string { return "RacerReady" }

func pending() component.Result {
	return component.NotReadyAfter("Initializing", "waiting for Racer installation", 5*time.Second)
}

func objectKey(env *component.Env, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: env.Namespace, Name: name}
}

func add(plan *component.Plan, kind component.OpKind, obj client.Object) {
	plan.Add(component.Operation{Kind: kind, Object: component.ToUnstructured(obj), Component: name})
}

// Plan only reads state. A zero-cache pass retains resources without reconciling
// them, allowing deliberate manual removal. Neither a cache nor a Site owns the
// installation's identity or durable counters.
func (Component) Plan(ctx context.Context, env *component.Env, _ []machinav1.Site) (*component.Plan, component.Result, error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()
	if len(caches.Items) == 0 {
		return plan, component.Disabled("no ClusterCaches; Racer resources are retained"), nil
	}

	marker, err := planIdentity(ctx, env, plan)
	if err != nil {
		return nil, component.Result{}, err
	}

	if marker == nil {
		return plan, pending(), nil
	}

	fresh := marker.Data["state"] == "fresh" && !ptr.Deref(marker.Immutable, false)

	consumed := marker.Data["state"] == "consumed" && ptr.Deref(marker.Immutable, false)
	if !fresh && !consumed {
		return nil, component.Result{}, fmt.Errorf("invalid Racer installation state")
	}

	if fresh {
		err := env.LiveReader().Get(ctx, objectKey(env, versionName), &corev1.ConfigMap{})
		if !apierrors.IsNotFound(err) {
			return nil, component.Result{}, fmt.Errorf("fresh Racer marker requires absent version state (read: %v)", err)
		}
	} else if err := racercore.ValidateInstallation(ctx, env.LiveReader(), env.Namespace, marker.Data["cluster"]); err != nil {
		// A running initializer may be between marker consumption and counter
		// creation. Requeue, but never issue another initialization attempt.
		job := &batchv1.Job{}
		if getErr := env.LiveReader().Get(ctx, objectKey(env, jobName), job); getErr == nil &&
			job.Annotations[installationAnnotation] == string(marker.UID) && job.Status.Active > 0 && job.DeletionTimestamp == nil {
			return plan, pending(), nil
		}

		return nil, component.Result{}, fmt.Errorf("racer durable state: %w", err)
	}

	secret, err := planTLS(ctx, env, plan, fresh)
	if err != nil {
		return nil, component.Result{}, err
	}

	if secret == nil {
		return plan, pending(), nil
	}

	if err := runtimePlan(env, plan, marker.Data["cluster"], secret, consumed); err != nil {
		return nil, component.Result{}, err
	}

	if fresh {
		job := &batchv1.Job{}

		err := env.LiveReader().Get(ctx, objectKey(env, jobName), job)
		if apierrors.IsNotFound(err) {
			op := component.Operation{Kind: component.OpCreateIfAbsent, Object: component.ToUnstructured(initializationJob(env, marker.UID)), Component: name}
			// Jobs are not an inferred workload tier. Declare all prerequisites
			// explicitly so no failed RBAC/config write can launch initialization.
			for _, dependency := range plan.Operations {
				op.DependsOn = append(op.DependsOn, dependency.Ref())
			}

			plan.Add(op)
		} else if err != nil {
			return nil, component.Result{}, err
		} else if job.Annotations[installationAnnotation] != string(marker.UID) || job.DeletionTimestamp != nil || job.Status.Failed > 0 || job.Status.Succeeded > 0 {
			return nil, component.Result{}, fmt.Errorf("racer initializer failed or does not match the fresh installation; inspect job/%s", jobName)
		}

		return plan, pending(), nil
	}

	return plan, component.ReconciledAfter("Racer installation reconciled", time.Hour), nil
}

func runtimePlan(env *component.Env, plan *component.Plan, cluster string, secret *corev1.Secret, ready bool) error {
	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"rbac.yaml", "config.yaml", "controller.yaml"}, nil)
	if err != nil {
		return err
	}

	var configHash string

	for _, obj := range objects {
		switch obj.GetKind() {
		case "ConfigMap":
			// These are installation invariants, not standalone render inputs.
			values := map[string]string{
				"RACER_CLUSTER_ID":                  cluster,
				"RACER_DATAPLANE_IMAGE":             env.Config.Image("racer-dataplane"),
				"RACER_CONTROL_URL":                 "https://racer-controller." + env.Namespace + ".svc:8443",
				"RACER_BOOTSTRAP_TRUST_CONFIGMAP":   trustName,
				"RACER_INSTALLATION_CONFIGMAP_NAME": markerName,
				"RACER_VERSION_CONFIGMAP_NAME":      versionName,
			}
			for key, value := range values {
				if err := unstructured.SetNestedField(obj.Object, value, "data", key); err != nil {
					return err
				}
			}

			cm := &corev1.ConfigMap{}
			if err := env.Scheme.Convert(obj, cm, nil); err != nil {
				return err
			}

			configHash = component.ConfigMapPayloadHash(cm)
		case "Deployment":
			if !ready {
				continue
			}

			if err := component.SetNamedContainerImage(obj, "controller", env.Config.Image(controllerName)); err != nil {
				return err
			}

			annotations := map[string]string{
				"unbounded-cloud.io/racer-config-hash": configHash,
				"unbounded-cloud.io/racer-tls-hash":    fmt.Sprintf("%x", sha256.Sum256(secret.Data[corev1.TLSCertKey])),
			}
			if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
				return err
			}
		}

		plan.Add(component.Operation{Kind: component.OpApply, Object: obj, Component: name, Overridable: obj.GetKind() == "Deployment"})
	}

	add(plan, component.OpApply, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace},
		Data:       map[string]string{"ca.crt": string(secret.Data["ca.crt"])},
	})

	return nil
}

func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(claimName, markerName, configName, trustName))))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(tlsName))))
	b.Watches(&batchv1.Job{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(jobName))))
}
