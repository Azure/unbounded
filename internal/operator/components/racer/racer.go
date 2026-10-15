// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer provisions Racer on demand from ClusterCaches.
package racer

import (
	"context"
	"fmt"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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
	"github.com/Azure/unbounded/internal/racer/members"
)

const (
	name              = "racer"
	controllerName    = "racer-controller"
	claimName         = "racer-operator-installation"
	markerName        = "racer-installation"
	versionName       = "racer-version"
	configName        = "racer-config"
	dataplaneConfig   = "racer-dataplane-config"
	dataplaneName     = members.DataplaneDaemonSetName
	tlsName           = "racer-controller-tls"
	trustName         = "racer-bootstrap-trust"
	managerAnnotation = "unbounded-cloud.io/racer-manager"
	claimAnnotation   = "unbounded-cloud.io/racer-operator-claim-uid"
	// Authority protocol key, not operator ownership metadata.
	installationAnnotation = "racer.unbounded-cloud.io/installation-uid"
)

// Component is the sole owner of Racer workloads and deployment configuration,
// including the dataplane DaemonSets.
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

	cfg, tuning, err := runtimePlan(ctx, env, plan, marker, secret)
	if err != nil {
		return nil, component.Result{}, err
	}

	bootstrap, needed, err := planBootstrap(ctx, env, plan, marker)
	if err != nil {
		return nil, component.Result{}, err
	}

	if needed {
		return bootstrap, pending(), nil
	}

	// Bootstrap is complete only once the marker is consumed and credentials
	// are committed. Dataplanes enroll against them, so they start only now.
	result, err := planDataplanes(ctx, env, plan, cfg, tuning, marker.UID)
	if err != nil {
		return nil, component.Result{}, err
	}

	return plan, result, nil
}

func decodeRuntimeManifests(env *component.Env) ([]*unstructured.Unstructured, error) {
	return env.DecodeManifestFiles(manifests.Manifests, []string{"node-restriction.yaml", "rbac.yaml", "config.yaml", "controller-pdb.yaml", "controller.yaml"}, nil)
}

func runtimePlan(ctx context.Context, env *component.Env, plan *component.Plan, marker *corev1.ConfigMap, secret *corev1.Secret) (members.Config, *corev1.ConfigMap, error) {
	objects, err := decodeRuntimeManifests(env)
	if err != nil {
		return members.Config{}, nil, err
	}

	var (
		configHash    string
		cfg           members.Config
		prerequisites []component.ObjectRef
	)

	for _, obj := range objects {
		switch obj.GetKind() {
		case "ConfigMap":
			values := map[string]string{
				"RACER_CLUSTER_ID":                  marker.Data["cluster"],
				"RACER_DATAPLANE_IMAGE":             env.Config.Image(dataplaneName),
				"RACER_CONTROL_URL":                 "https://" + controllerName + "." + env.Namespace + ".svc:8443",
				"RACER_BOOTSTRAP_TRUST_CONFIGMAP":   trustName,
				"RACER_INSTALLATION_CONFIGMAP_NAME": markerName,
				"RACER_VERSION_CONFIGMAP_NAME":      versionName,
				"RACER_CREDENTIALS_SECRET_NAME":     credentialsName,
				"RACER_DAEMONSET_NAME":              dataplaneName,
				"RACER_DATAPLANE_SERVICE_ACCOUNT":   dataplaneName,
				"RACER_CONTROLLER_SERVICE_ACCOUNT":  controllerName,
				"RACER_REPLICATION_TOKEN_FILE":      "/var/run/secrets/racer-controller/token",
				"RACER_REPLICATION_TRUST_FILE":      "/etc/racer/tls/ca.crt",
				"RACER_REPLICATION_SERVER_NAME":     controllerName + "." + env.Namespace + ".svc",
				"RACER_REPLICATION_PORT":            "8443",
			}
			for key, value := range values {
				if err := unstructured.SetNestedField(obj.Object, value, "data", key); err != nil {
					return members.Config{}, nil, err
				}
			}

			cm := &corev1.ConfigMap{}
			if err := env.Scheme.Convert(obj, cm, nil); err != nil {
				return members.Config{}, nil, err
			}

			bindRuntime(cm, marker.UID)

			cm, err = preservedConfig(ctx, env, plan, cm, values)
			if err != nil {
				return members.Config{}, nil, err
			}

			configHash = component.ConfigMapPayloadHash(cm)

			lookup := func(key string) (string, bool) {
				if key == "POD_NAMESPACE" {
					return env.Namespace, true
				}

				value, ok := cm.Data[key]

				return value, ok
			}

			if _, err = racercore.ConfigFromLookup(lookup); err != nil {
				return members.Config{}, nil, err
			}

			// Dataplane wiring shares racer-config; validate it before any apply.
			cfg, err = members.ConfigFromLookup(lookup)
			if err != nil {
				return members.Config{}, nil, err
			}

			prerequisites = append(prerequisites, component.RefOf(component.ToUnstructured(cm)))

			continue
		case "Deployment":
			if err := component.SetNamedContainerImage(obj, "controller", env.Config.Image(controllerName)); err != nil {
				return members.Config{}, nil, err
			}

			if err := unstructured.SetNestedStringMap(obj.Object, map[string]string{"unbounded-cloud.io/racer-config-hash": configHash}, "spec", "template", "metadata", "annotations"); err != nil {
				return members.Config{}, nil, err
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
			return members.Config{}, nil, err
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
		return members.Config{}, nil, err
	}

	plan.Add(op)

	tuning, err := dataplaneConfigPlan(ctx, env, plan, marker.UID)
	if err != nil {
		return members.Config{}, nil, err
	}

	return cfg, tuning, nil
}

// The tuning ConfigMap preserves administrator data; edits roll out to the
// dataplanes through the payload hash.
func dataplaneConfigPlan(ctx context.Context, env *component.Env, plan *component.Plan, installation types.UID) (*corev1.ConfigMap, error) {
	defaults, err := env.DefaultConfigMap(manifests.Manifests, dataplaneConfig, name)
	if err != nil {
		return nil, err
	}

	bindRuntime(defaults, installation)

	return preservedConfig(ctx, env, plan, defaults, nil)
}

func planDataplanes(ctx context.Context, env *component.Env, plan *component.Plan, cfg members.Config, tuning *corev1.ConfigMap, installation types.UID) (component.Result, error) {
	sets, result, err := migrationPlan(ctx, env, cfg)
	if err != nil {
		return component.Result{}, err
	}

	for _, ds := range sets {
		ds.Spec.Template.Annotations = map[string]string{"unbounded-cloud.io/racer-config-hash": component.ConfigMapPayloadHash(tuning)}
		container := &ds.Spec.Template.Spec.Containers[0]
		container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfig}}}}
		// Scheduling reservation, not a claim that admission budgets bound RSS.
		// TLS, metadata, allocator overhead and filesystem cache are additional.
		container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}

		// Override affinity is intersected with every operator term. Overrides
		// must name the host or pod-network workload explicitly.
		op := component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(ds), Component: name, Overridable: true}
		for _, dependency := range plan.Operations {
			if dependency.Object.GetKind() != "Deployment" {
				op.DependsOn = append(op.DependsOn, dependency.Ref())
			}
		}

		op, err = ownedRuntimeOperation(ctx, env, op, installation)
		if err != nil {
			return component.Result{}, err
		}

		plan.Add(op)
	}

	return result, nil
}

func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&admissionv1.ValidatingAdmissionPolicy{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&admissionv1.ValidatingAdmissionPolicyBinding{}, env.RequestSingleton(), builder.WithPredicates(guardPredicate()))
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(dataplaneName, members.PodNetworkDaemonSetName))))
	b.Watches(&corev1.Pod{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool { return obj.GetNamespace() == env.Namespace && migrationPod(obj) })))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(startupConfigPredicate(env)))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(tlsName))))
}

func startupConfigPredicate(env *component.Env) predicate.Predicate {
	// Counter updates do not require workload reconciliation.
	version := env.InNamespaceNamed(versionName)

	return predicate.Or(
		env.ManagedConfigPredicate(env.InNamespaceNamed(claimName, markerName, configName, dataplaneConfig, trustName)),
		predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return version(e.Object) },
			DeleteFunc:  func(e event.DeleteEvent) bool { return version(e.Object) },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		},
	)
}
