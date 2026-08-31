// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package tokenrefresher implements the bootstrap token refresher singleton.
package tokenrefresher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/builder"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	tokenrefreshermanifests "github.com/Azure/unbounded/deploy/token-refresher"
	"github.com/Azure/unbounded/internal/operator/component"
)

const (
	name                 = "token-refresher"
	clusterSiteName      = "cluster"
	configHashAnnotation = "unbounded-cloud.io/token-refresher-config-hash"
)

// Component reconciles the token-refresher cluster singleton.
type Component struct{}

// New returns the token-refresher cluster component.
func New() component.ClusterComponent { return Component{} }

func (Component) Name() string          { return name }
func (Component) ConditionType() string { return "TokenRefresherReady" }

// EnabledFor reports whether a non-cluster Site enables token-refresher. The
// component defaults on unless enabled is explicitly false.
func EnabledFor(site *unboundedv1alpha3.Site) bool {
	if site.Name == clusterSiteName {
		return false
	}

	spec := site.Spec.Components.TokenRefresher

	return spec == nil || spec.Enabled == nil || *spec.Enabled
}

// Plan applies the singleton while any eligible Site enables it, and explicitly
// removes every resource owned by the component otherwise.
func (c Component) Plan(_ context.Context, env *component.Env, sites []unboundedv1alpha3.Site) (*component.Plan, component.Result, error) {
	for i := range sites {
		if EnabledFor(&sites[i]) {
			return c.installPlan(env)
		}
	}

	return cleanupPlan(env.Namespace), component.Disabled("no non-cluster site enables token-refresher"), nil
}

func (c Component) installPlan(env *component.Env) (*component.Plan, component.Result, error) {
	var configHash string

	objects, err := env.DecodeManifestFS(tokenrefreshermanifests.Manifests, func(obj *unstructured.Unstructured) error {
		if obj.GetKind() == "ConfigMap" && obj.GetName() == name {
			payload, found, err := unstructured.NestedMap(obj.Object, "data")
			if err != nil {
				return fmt.Errorf("read token-refresher config: %w", err)
			}

			if !found {
				return fmt.Errorf("token-refresher ConfigMap has no data")
			}

			encoded, err := json.Marshal(payload)
			if err != nil {
				return fmt.Errorf("encode token-refresher config: %w", err)
			}

			sum := sha256.Sum256(encoded)
			configHash = hex.EncodeToString(sum[:])
		}

		if obj.GetKind() == "Deployment" && obj.GetName() == name {
			if err := component.SetPodSpecImages(obj, env.Config.Image(name)); err != nil {
				return fmt.Errorf("set token-refresher image: %w", err)
			}

			if configHash == "" {
				return fmt.Errorf("token-refresher Deployment decoded before its ConfigMap")
			}

			if err := unstructured.SetNestedField(obj.Object, configHash,
				"spec", "template", "metadata", "annotations", configHashAnnotation); err != nil {
				return fmt.Errorf("set token-refresher config hash: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, component.Result{}, err
	}

	plan := component.NewPlan()

	for _, obj := range objects {
		op := component.Operation{Kind: component.OpApply, Object: obj, Component: c.Name()}
		if obj.GetKind() == "Deployment" && obj.GetName() == name {
			op.Overridable = true
		}

		plan.Add(op)
	}

	return plan, component.Reconciled(), nil
}

func cleanupPlan(namespace string) *component.Plan {
	plan := component.NewPlan()

	for _, obj := range []struct {
		kind      string
		api       string
		namespace string
	}{
		{kind: "Deployment", api: "apps/v1", namespace: namespace},
		{kind: "ConfigMap", api: "v1", namespace: namespace},
		{kind: "RoleBinding", api: "rbac.authorization.k8s.io/v1", namespace: namespace},
		{kind: "Role", api: "rbac.authorization.k8s.io/v1", namespace: namespace},
		{kind: "RoleBinding", api: "rbac.authorization.k8s.io/v1", namespace: "kube-system"},
		{kind: "Role", api: "rbac.authorization.k8s.io/v1", namespace: "kube-system"},
		{kind: "ClusterRoleBinding", api: "rbac.authorization.k8s.io/v1"},
		{kind: "ClusterRole", api: "rbac.authorization.k8s.io/v1"},
		{kind: "ServiceAccount", api: "v1", namespace: namespace},
	} {
		u := &unstructured.Unstructured{}
		u.SetAPIVersion(obj.api)
		u.SetKind(obj.kind)
		u.SetName(name)
		u.SetNamespace(obj.namespace)
		plan.Add(component.Operation{Kind: component.OpDelete, Object: u, Component: name})
	}

	return plan
}

// SetupWatches reconciles drift and deletion of the managed workload and config.
func (Component) SetupWatches(b *builder.Builder, env *component.Env) {
	b.Watches(&appsv1.Deployment{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedWorkloadPredicate(env.InNamespaceNamed(name))))
	b.Watches(&corev1.ConfigMap{}, env.RequestSingleton(),
		builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(name))))
}
