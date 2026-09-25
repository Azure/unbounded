// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer installs Racer while at least one ClusterCache exists.
package racer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
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
	"github.com/Azure/unbounded/internal/racer/wire"
)

const (
	name           = "racer"
	controllerName = "racer-controller"
	markerName     = "racer-installation"
	configName     = "racer-config"
	jobName        = "racer-initialize"
	tlsName        = "racer-controller-tls"
	trustName      = "racer-bootstrap-trust"
)

// Component manages controller prerequisites and leaves dataplane ownership to Racer.
type Component struct{}

// New returns the cluster-wide Racer component.
func New() component.ClusterComponent   { return Component{} }
func (Component) Name() string          { return name }
func (Component) ConditionType() string { return "RacerReady" }

func pending() component.Result {
	return component.NotReadyAfter("Initializing", "waiting for Racer installation", 5*time.Second)
}

func add(plan *component.Plan, kind component.OpKind, obj client.Object) {
	plan.Add(component.Operation{Kind: kind, Object: component.ToUnstructured(obj), Component: name})
}

func objectKey(env *component.Env, name string) client.ObjectKey {
	return client.ObjectKey{Namespace: env.Namespace, Name: name}
}

// Plan never writes or removes durable state. A zero-cache pass is deliberately
// empty even when retained workloads exist, allowing administrators to remove them.
func (Component) Plan(ctx context.Context, env *component.Env, _ []machinav1.Site) (*component.Plan, component.Result, error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()
	if len(caches.Items) == 0 {
		return plan, component.Disabled("no ClusterCaches; Racer resources are retained and may be removed manually"), nil
	}

	marker := &corev1.ConfigMap{}

	err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: markerName}, marker)
	if apierrors.IsNotFound(err) {
		if err := checkNewInstallation(ctx, env); err != nil {
			return nil, component.Result{}, err
		}
		// Commit the random identity alone, then re-read the winning Create on
		// the next pass. No dependent object uses an uncommitted identity.
		add(plan, component.OpCreateIfAbsent, &corev1.ConfigMap{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
			ObjectMeta: metav1.ObjectMeta{Namespace: env.Namespace, Name: markerName},
			Data:       map[string]string{"cluster": uuid.NewString(), "version_configmap": "racer-version", "state": "fresh"},
		})

		return plan, pending(), nil
	}

	if err != nil {
		return nil, component.Result{}, err
	}

	cluster := marker.Data["cluster"]
	if !wire.ValidUUID(cluster) || marker.UID == "" || marker.DeletionTimestamp != nil || marker.Data["version_configmap"] != "racer-version" {
		return nil, component.Result{}, fmt.Errorf("invalid Racer installation marker; restore consistent durable state")
	}

	fresh := marker.Data["state"] == "fresh" && !ptr.Deref(marker.Immutable, false)

	consumed := marker.Data["state"] == "consumed" && ptr.Deref(marker.Immutable, false)
	if !fresh && !consumed {
		return nil, component.Result{}, fmt.Errorf("invalid Racer installation state")
	}

	if fresh {
		version := &corev1.ConfigMap{}

		err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: "racer-version"}, version)
		if !apierrors.IsNotFound(err) {
			return nil, component.Result{}, fmt.Errorf("fresh Racer marker requires absent version state: %v", err)
		}
	}

	ready := false

	if consumed {
		if err := racercore.ValidateInstallation(ctx, env.LiveReader(), env.Namespace, cluster); err != nil {
			// The initializer may still be between consuming the marker and its
			// single counter Create. Never launch another initializer here.
			job := &batchv1.Job{}
			if getErr := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: jobName}, job); getErr == nil && job.Status.Active > 0 {
				return plan, pending(), nil
			}

			return nil, component.Result{}, fmt.Errorf("racer durable state: %w", err)
		}

		ready = true
	}

	secret, err := planTLS(ctx, env, plan)
	if err != nil {
		return nil, component.Result{}, err
	}

	if secret == nil {
		return plan, pending(), nil
	}

	if err := runtimePlan(env, plan, cluster, secret, ready); err != nil {
		return nil, component.Result{}, err
	}

	if fresh {
		job := &batchv1.Job{}

		err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: jobName}, job)
		if apierrors.IsNotFound(err) {
			add(plan, component.OpCreateIfAbsent, initializationJob(env, marker.UID))
		} else if err != nil {
			return nil, component.Result{}, err
		} else if job.Annotations["racer.unbounded-cloud.io/installation-uid"] != string(marker.UID) || job.Status.Failed > 0 || job.Status.Succeeded > 0 {
			return nil, component.Result{}, fmt.Errorf("racer initializer failed or does not match the fresh installation; inspect job/%s", jobName)
		}

		return plan, pending(), nil
	}

	return plan, component.ReconciledAfter("Racer installation reconciled", time.Hour), nil
}

// Refuse to generate a new identity over evidence of an earlier installation.
func checkNewInstallation(ctx context.Context, env *component.Env) error {
	objects := []client.Object{
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "racer-version"}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: tlsName}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-issuer"}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "racer-keyring"}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: jobName}},
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: controllerName}},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "racer-dataplane"}},
	}
	for _, obj := range objects {
		err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: obj.GetName()}, obj)
		if err == nil {
			return fmt.Errorf("racer installation marker missing but %s exists; restore consistent durable state", obj.GetName())
		}

		if !apierrors.IsNotFound(err) {
			return err
		}
	}

	return nil
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
			if err := unstructured.SetNestedField(obj.Object, cluster, "data", "RACER_CLUSTER_ID"); err != nil {
				return err
			}

			if err := unstructured.SetNestedField(obj.Object, env.Config.Image("racer-dataplane"), "data", "RACER_DATAPLANE_IMAGE"); err != nil {
				return err
			}

			if err := unstructured.SetNestedField(obj.Object, "https://racer-controller."+env.Namespace+".svc:8443", "data", "RACER_CONTROL_URL"); err != nil {
				return err
			}

			data, err := json.Marshal(obj.Object["data"])
			if err != nil {
				return err
			}

			configHash = fmt.Sprintf("%x", sha256.Sum256(data))
		case "Deployment":
			if !ready {
				continue
			}

			if err := component.SetPodSpecImages(obj, env.Config.Image(controllerName)); err != nil {
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
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton())
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(markerName, configName, trustName))))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(tlsName))))
	b.Watches(&batchv1.Job{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(jobName))))
}
