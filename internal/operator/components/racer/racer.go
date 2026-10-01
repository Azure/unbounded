// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package racer provisions Racer on demand from cluster-scoped ClusterCaches.
package racer

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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
	name                   = "racer"
	controllerName         = "racer-controller"
	claimName              = "racer-operator-installation"
	markerName             = "racer-installation"
	versionName            = "racer-version"
	configName             = "racer-config"
	dataplaneConfigName    = "racer-dataplane-config"
	dataplaneName          = "racer-dataplane"
	tlsName                = "racer-controller-tls"
	trustName              = "racer-bootstrap-trust"
	managerAnnotation      = "racer.unbounded-cloud.io/manager"
	claimAnnotation        = "racer.unbounded-cloud.io/operator-claim-uid"
	installationAnnotation = "racer.unbounded-cloud.io/installation-uid"
)

// Component is the sole owner of Racer workloads and deployment configuration.
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

// Plan only reads state. A zero-cache pass maintains established serving TLS but
// does not repair workloads or recreate deleted credentials. Neither a cache nor
// a Site owns the installation's identity or durable counters.
func (Component) Plan(ctx context.Context, env *component.Env, _ []machinav1.Site) (*component.Plan, component.Result, error) {
	return planAt(ctx, env, time.Now())
}

func planAt(ctx context.Context, env *component.Env, now time.Time) (*component.Plan, component.Result, error) {
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
		err := env.LiveReader().Get(ctx, objectKey(env, versionName), &corev1.ConfigMap{})
		if !apierrors.IsNotFound(err) {
			return nil, component.Result{}, fmt.Errorf("fresh Racer marker requires absent version state (read: %v)", err)
		}
	} else if err := racercore.ValidateInstallation(ctx, env.LiveReader(), env.Namespace, marker.Data["cluster"]); err != nil {
		// Startup may be in its one-shot Create gap. Do not deploy dataplanes or
		// repair durable state; a later reconcile can observe successful creation.
		return nil, component.Result{}, fmt.Errorf("racer durable state: %w", err)
	}

	secret, err := planTLSAt(ctx, env, plan, fresh, now)
	if err != nil {
		return nil, component.Result{}, err
	}

	if secret == nil {
		return plan, pending(), nil
	}

	result := component.ReconciledAfter("Racer installation reconciled", time.Hour)
	if err := runtimePlan(ctx, env, plan, marker.Data["cluster"], secret, consumed, &result); err != nil {
		return nil, component.Result{}, err
	}

	if fresh {
		return plan, pending(), nil
	}

	return plan, result, nil
}

func runtimePlan(ctx context.Context, env *component.Env, plan *component.Plan, cluster string, secret *corev1.Secret, ready bool, result *component.Result) error {
	objects, err := env.DecodeManifestFiles(manifests.Manifests, []string{"create-restriction.yaml", "rbac.yaml", "config.yaml", "controller.yaml"}, nil)
	if err != nil {
		return err
	}

	var (
		configHash    string
		cfg           racercore.WorkloadConfig
		prerequisites []component.ObjectRef
	)

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
				"RACER_ISSUER_SECRET_NAME":          "racer-issuer",
				"RACER_KEYRING_SECRET_NAME":         "racer-keyring",
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
					return err
				}
			}

			cm := &corev1.ConfigMap{}
			if err := env.Scheme.Convert(obj, cm, nil); err != nil {
				return err
			}

			cm, err = preservedConfig(ctx, env, plan, cm, values)
			if err != nil {
				return err
			}

			configHash = component.ConfigMapPayloadHash(cm)

			cfg, err = racercore.WorkloadConfigFromLookup(func(key string) (string, bool) {
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

			annotations := map[string]string{
				"unbounded-cloud.io/racer-config-hash": configHash,
			}
			if err := unstructured.SetNestedStringMap(obj.Object, annotations, "spec", "template", "metadata", "annotations"); err != nil {
				return err
			}
		}

		op := component.Operation{Kind: component.OpApply, Object: obj, Component: name, Overridable: obj.GetKind() == "Deployment"}
		if obj.GetKind() == "Deployment" {
			// Every security/config prerequisite must succeed before startup can
			// consume the permanent marker, including during a fresh installation.
			for _, dependency := range plan.Operations {
				op.DependsOn = append(op.DependsOn, dependency.Ref())
			}
		}

		if obj.GetKind() == "ValidatingAdmissionPolicy" || obj.GetKind() == "ValidatingAdmissionPolicyBinding" {
			prerequisites = append(prerequisites, op.Ref())
		} else if obj.GetKind() == "RoleBinding" || obj.GetKind() == "Deployment" {
			op.DependsOn = append(op.DependsOn, prerequisites...)
		}

		plan.Add(op)
	}

	add(plan, component.OpApply, &corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace},
		Data:       map[string]string{"ca.crt": string(secret.Data["ca.crt"]) + string(secret.Data[previousCAKey])},
	})

	defaults, err := env.DefaultConfigMap(manifests.Manifests, dataplaneConfigName, name)
	if err != nil {
		return err
	}

	tuning, err := preservedConfig(ctx, env, plan, defaults, nil)
	if err != nil {
		return err
	}

	if ready {
		sets, migration, err := migrationPlan(ctx, env, cfg)
		if err != nil {
			return err
		}

		*result = migration

		for _, ds := range sets {
			ds.Spec.Template.Annotations = map[string]string{"unbounded-cloud.io/racer-config-hash": component.ConfigMapPayloadHash(tuning)}
			container := &ds.Spec.Template.Spec.Containers[0]
			container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfigName}}}}
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

			plan.Add(op)
		}
	}

	return nil
}

func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&racerv1.ClusterCache{}, env.RequestSingleton(), builder.WithPredicates(predicate.GenerationChangedPredicate{}))
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(), builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(controllerName))))
	b.Watches(&appsv1.DaemonSet{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(dataplaneName, racercore.PodNetworkDaemonSetName))))
	b.Watches(&corev1.Pod{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool { return obj.GetNamespace() == env.Namespace && migrationPod(obj) })))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(), builder.WithPredicates(startupConfigPredicate(env)))
	b.Watches(&corev1.Secret{}, env.RequestSingleton(), builder.WithPredicates(predicate.NewPredicateFuncs(env.InNamespaceNamed(tlsName))))
}

func startupConfigPredicate(env *component.Env) predicate.Predicate {
	// Observe startup completion/loss without applying workloads on every normal
	// topology counter update. Other configuration retains its payload predicate.
	version := env.InNamespaceNamed(versionName)

	return predicate.Or(
		env.ManagedConfigPredicate(env.InNamespaceNamed(claimName, markerName, configName, dataplaneConfigName, trustName)),
		predicate.Funcs{
			CreateFunc:  func(e event.CreateEvent) bool { return version(e.Object) },
			DeleteFunc:  func(e event.DeleteEvent) bool { return version(e.Object) },
			UpdateFunc:  func(event.UpdateEvent) bool { return false },
			GenericFunc: func(event.GenericEvent) bool { return false },
		},
	)
}
