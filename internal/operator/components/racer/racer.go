// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer provisions the Racer controller on demand from ClusterCaches.
package racer

import (
	"context"
	"fmt"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
)

const (
	name              = "racer"
	controllerName    = "racer-controller"
	claimName         = "racer-operator-installation"
	markerName        = "racer-installation"
	versionName       = "racer-version"
	configName        = "racer-config"
	tlsName           = "racer-controller-tls"
	trustName         = "racer-bootstrap-trust"
	managerAnnotation = "unbounded-cloud.io/racer-manager"
	claimAnnotation   = "unbounded-cloud.io/racer-operator-claim-uid"
	// Authority protocol key, not operator ownership metadata.
	installationAnnotation = "racer.unbounded-cloud.io/installation-uid"
)

// Component owns controller deployment inputs, not external dataplane workloads.
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

// Plan only reads state. With no caches it maintains established TLS, but does
// not repair workloads or recreate credentials. No cache or Site owns identity.
func (Component) Plan(ctx context.Context, env *component.Env, _ []machinav1.Site) (*component.Plan, component.Result, error) {
	return planAt(ctx, env, time.Now())
}

func planAt(ctx context.Context, env *component.Env, now time.Time) (*component.Plan, component.Result, error) {
	// Maintain only bound, established TLS before unrelated admission, cache,
	// or durable-state checks. This cannot initialize identity or repair workloads.
	tlsPlan, _, tlsErr := planRetainedTLS(ctx, env, now)
	guardPlan, stop, guardErr := planGuardContainment(ctx, env)
	guardResult := component.NotReadyAfter("AdmissionGuardUnavailable", "waiting for Racer admission guards and permission containment", 5*time.Second)

	if stop && guardErr == nil {
		// Containment must not wait for TLS writes to succeed. These operations
		// have no dependencies on each other and never grant new permissions.
		if tlsErr == nil {
			for _, op := range tlsPlan.Operations {
				op.FailureDomain = "retained-tls"
				guardPlan.Add(op)
			}
		}

		return guardPlan, guardResult, nil
	}

	if tlsErr != nil {
		return nil, component.Result{}, tlsErr
	}

	if tlsPlan.Len() != 0 && tlsPlan.Operations[0].Object.GetName() == tlsName {
		return tlsPlan, component.NotReadyAfter("TLSMaintenance", "updating Racer serving TLS and trust", 5*time.Second), nil
	}

	plan, result, err := guardPlan, guardResult, guardErr
	if err == nil {
		plan, result, err = planRuntimeAt(ctx, env, now)
	}

	if tlsPlan.Len() != 0 && err != nil {
		// Before the first runtime deployment there may be no trust to repair.
		// Keep startup errors visible rather than starting a new publication.
		if tlsPlan.Operations[0].Kind == component.OpCreateIfAbsent {
			if readErr := env.LiveReader().Get(ctx, objectKey(env, controllerName), &appsv1.Deployment{}); readErr != nil {
				return plan, result, err
			}
		}

		return tlsPlan, component.NotReadyAfter("TLSMaintenance", "updating Racer serving trust before runtime repair", 5*time.Second), nil
	}

	return plan, result, err
}

func planRuntimeAt(ctx context.Context, env *component.Env, now time.Time) (*component.Plan, component.Result, error) {
	caches := &racerv1.ClusterCacheList{}
	if err := env.LiveReader().List(ctx, caches, client.Limit(1)); err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	if len(caches.Items) == 0 {
		return planRetainedTLS(ctx, env, now)
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
		version := &corev1.ConfigMap{}

		err := env.LiveReader().Get(ctx, objectKey(env, versionName), version)
		if !apierrors.IsNotFound(err) {
			// The execution phase finishes the staged startup commitment.
			if err != nil || marker.Data["initialization_protocol"] != "staged-v1" || version.Annotations["racer.unbounded-cloud.io/initialization"] != "staged-v1" || version.Annotations[installationAnnotation] != string(marker.UID) {
				return nil, component.Result{}, fmt.Errorf("fresh Racer marker requires absent or bound staged version state (read: %v)", err)
			}
		}
	}

	secret, err := planTLSAt(ctx, env, plan, marker.UID, fresh, now)
	if err != nil {
		return nil, component.Result{}, err
	}

	if secret == nil {
		return plan, pending(), nil
	}

	if err := runtimePlan(ctx, env, plan, marker, secret); err != nil {
		return nil, component.Result{}, err
	}

	bootstrap, needed, err := planBootstrap(ctx, env, plan, marker)
	if err != nil {
		return nil, component.Result{}, err
	}

	if needed {
		return bootstrap, pending(), nil
	}

	return plan, component.ReconciledAfter("Racer controller installation reconciled", time.Hour), nil
}

func decodeRuntimeManifests(env *component.Env) ([]*unstructured.Unstructured, error) {
	return env.DecodeManifestFiles(manifests.Manifests, []string{"node-restriction.yaml", "rbac.yaml", "config.yaml", "controller-pdb.yaml", "controller.yaml"}, nil)
}

func runtimePlan(ctx context.Context, env *component.Env, plan *component.Plan, marker *corev1.ConfigMap, secret *corev1.Secret) error {
	objects, err := decodeRuntimeManifests(env)
	if err != nil {
		return err
	}

	var (
		configHash    string
		prerequisites []component.ObjectRef
	)

	for _, obj := range objects {
		switch obj.GetKind() {
		case "ConfigMap":
			values := map[string]string{
				"RACER_CLUSTER_ID":                  marker.Data["cluster"],
				"RACER_INSTALLATION_CONFIGMAP_NAME": markerName,
				"RACER_VERSION_CONFIGMAP_NAME":      versionName,
				"RACER_CREDENTIALS_SECRET_NAME":     credentialsName,
				"RACER_DAEMONSET_NAME":              "racer-dataplane",
				"RACER_DATAPLANE_SERVICE_ACCOUNT":   "racer-dataplane",
				"RACER_CONTROLLER_SERVICE_ACCOUNT":  controllerName,
				"RACER_REPLICATION_TOKEN_FILE":      "/var/run/secrets/racer-controller/token",
				"RACER_REPLICATION_TRUST_FILE":      "/etc/racer/tls/ca.crt",
				"RACER_REPLICATION_SERVER_NAME":     controllerName + "." + env.Namespace + ".svc",
				"RACER_REPLICATION_PORT":            "8443",
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

			bindRuntime(cm, marker.UID)

			cm, err = preservedConfig(ctx, env, plan, cm, values)
			if err != nil {
				return err
			}

			configHash = component.ConfigMapPayloadHash(cm)

			_, err = racercore.ConfigFromLookup(func(key string) (string, bool) {
				if key == "POD_NAMESPACE" {
					return env.Namespace, true
				}

				value, ok := cm.Data[key]

				return value, ok
			})
			if err != nil {
				return err
			}

			prerequisites = append(prerequisites, component.RefOf(component.ToUnstructured(cm)))

			continue
		case "Deployment":
			if err := component.SetNamedContainerImage(obj, "controller", env.Config.Image(controllerName)); err != nil {
				return err
			}

			if err := unstructured.SetNestedStringMap(obj.Object, map[string]string{"unbounded-cloud.io/racer-config-hash": configHash}, "spec", "template", "metadata", "annotations"); err != nil {
				return err
			}
		}

		op := component.Operation{Kind: component.OpApply, Object: obj, Component: name, Overridable: obj.GetKind() == "Deployment"}
		if obj.GetKind() == "Deployment" {
			op.ValidateOverride = ValidateOverride
			// Security and configuration must succeed before startup consumes identity.
			for _, dependency := range plan.Operations {
				op.DependsOn = append(op.DependsOn, dependency.Ref())
			}
		}

		if obj.GetKind() == "ValidatingAdmissionPolicy" || obj.GetKind() == "ValidatingAdmissionPolicyBinding" {
			prerequisites = append(prerequisites, op.Ref())
		} else if obj.GetKind() == "RoleBinding" || obj.GetKind() == "ClusterRoleBinding" || obj.GetKind() == "Deployment" {
			op.DependsOn = append(op.DependsOn, prerequisites...)
		}

		op, err = ownedRuntimeOperation(ctx, env, op, marker.UID)
		if err != nil {
			return err
		}

		plan.Add(op)
	}

	trust := &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace},
		Data:       map[string]string{"ca.crt": string(servingRoots(secret))},
	}

	op, err := ownedRuntimeOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(trust), Component: name}, marker.UID)
	if err != nil {
		return err
	}

	plan.Add(op)

	return nil
}

func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&admissionv1.ValidatingAdmissionPolicy{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&admissionv1.ValidatingAdmissionPolicyBinding{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(startupConfigPredicate(env)))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(tlsName))))
}

func startupConfigPredicate(env *component.Env) predicate.Predicate {
	// Counter updates do not require workload reconciliation.
	version := env.InNamespaceNamed(versionName)

	return predicate.Or(
		env.ManagedConfigPredicate(env.InNamespaceNamed(claimName, markerName, configName, trustName)),
		predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return version(e.Object) },
			DeleteFunc:  func(e event.DeleteEvent) bool { return version(e.Object) },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		},
	)
}
