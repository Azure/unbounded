// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

// registryAuthMachinePredicate enqueues a gantry re-reconcile when a Machine's
// registry-auth policy (spec) or its reported registry-auth state (status
// condition) changes, so the operator re-reads the node-reported readiness. It
// ignores other Machine changes to avoid re-reconciling the whole cluster on
// unrelated updates.
func registryAuthMachinePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool {
			m, ok := ev.Object.(*v1alpha3.Machine)
			return ok && m.Spec.RegistryAuth != nil
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return false },
		GenericFunc: func(ev event.GenericEvent) bool { return false },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			oldMachine, okOld := ev.ObjectOld.(*v1alpha3.Machine)
			newMachine, okNew := ev.ObjectNew.(*v1alpha3.Machine)
			if !okOld || !okNew {
				return false
			}

			return !sameRegistryAuthPolicy(oldMachine.Spec.RegistryAuth, newMachine.Spec.RegistryAuth) ||
				registryAuthConditionChanged(oldMachine, newMachine)
		},
	}
}

// registryAuthConditionChanged reports whether the RegistryAuth condition on a
// Machine changed between two observations.
func registryAuthConditionChanged(oldMachine, newMachine *v1alpha3.Machine) bool {
	oldCond := apimeta.FindStatusCondition(oldMachine.Status.Conditions, v1alpha3.MachineConditionRegistryAuth)
	newCond := apimeta.FindStatusCondition(newMachine.Status.Conditions, v1alpha3.MachineConditionRegistryAuth)

	if (oldCond == nil) != (newCond == nil) {
		return true
	}

	if oldCond == nil {
		return false
	}

	return oldCond.Status != newCond.Status || oldCond.Reason != newCond.Reason
}

const (
	registryAuthName         = "gantry-acr-auth"
	registryAuthHash         = "unbounded-cloud.io/registry-auth-config-hash"
	registryAuthAudienceName = "gantry-acr-kubelet-token-audience"
)

// registryAuthPolicyFromConfig maps the operator's node-auth policy to the
// Machine.spec.registryAuth field the agent applies. An external (or absent)
// policy maps to nil: the agent then leaves the kubelet's authentication
// untouched and removes any managed provider it had installed.
func registryAuthPolicyFromConfig(cfg registryauth.Config) *v1alpha3.RegistryAuthPolicy {
	if cfg.Mode != registryauth.IdentityBindingMode {
		return nil
	}

	return &v1alpha3.RegistryAuthPolicy{
		Mode:         cfg.Mode,
		Registry:     cfg.Registry,
		ClientID:     cfg.ClientID,
		TenantID:     cfg.TenantID,
		SNIName:      cfg.SNIName,
		ProxyAddress: cfg.ProxyAddress,
		Audience:     cfg.Audience,
		CACert:       cfg.CACert,
	}
}

// planRegistryAuth delivers the node-auth policy to the agent on every eligible
// Flex node by writing Machine.spec.registryAuth, and reads back the node-
// reported Machine.Status.registryAuth condition to determine readiness. The
// legacy gantry-acr-auth DaemonSet that installed the provider per node is
// retired: the agent's isolation backend now owns the kubelet configuration, so
// the operator no longer needs a DaemonSet (and avoids a per-node pod at scale).
func planRegistryAuth(ctx context.Context, env *component.Env) ([]component.Operation, *component.Result, error) {
	var source corev1.ConfigMap
	if err := env.Client.Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: configName}, &source); apierrors.IsNotFound(err) {
		defaultConfig, err := env.DefaultConfigMap(gantrymanifests.Manifests, configName, "gantry")
		if err != nil {
			return nil, nil, err
		}

		source = *defaultConfig
	} else if err != nil {
		return nil, nil, err
	}

	cfg, err := registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
	if err != nil {
		return nil, nil, err
	}

	active := cfg.Mode == registryauth.IdentityBindingMode
	policy := registryAuthPolicyFromConfig(cfg)

	operations, machines, err := planRegistryAuthMachines(ctx, env, policy, active)
	if err != nil {
		return nil, nil, err
	}

	// Retire the legacy per-node installer DaemonSet (and its ServiceAccount)
	// once the agent path owns the configuration. The legacy ConfigMap marker
	// and its RBAC/Lease are cleaned up alongside.
	cleanup, err := planRetireLegacyRegistryAuth(ctx, env)
	if err != nil {
		return nil, nil, err
	}

	operations = append(operations, cleanup...)

	return operations, registryAuthResult(machines, active), nil
}

// planRegistryAuthMachines writes the policy to every eligible Flex node's
// Machine and returns the planned operations plus the observed Machines, so the
// caller can read back the node-reported readiness.
func planRegistryAuthMachines(ctx context.Context, env *component.Env, policy *v1alpha3.RegistryAuthPolicy, active bool) ([]component.Operation, []v1alpha3.Machine, error) {
	nodes, err := eligibleFlexNodes(ctx, env)
	if err != nil {
		return nil, nil, err
	}

	var (
		operations []component.Operation
		machines   []v1alpha3.Machine
	)

	for _, name := range nodes {
		var machine v1alpha3.Machine
		err := env.LiveReader().Get(ctx, client.ObjectKey{Name: name}, &machine)
		if apierrors.IsNotFound(err) {
			// A node without a Machine is not yet agent-backed; skip it. The
			// agent registers its Machine at bootstrap, so it will reconcile on
			// the next pass once the Machine exists.
			continue
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read Machine %s: %w", name, err)
		}

		machines = append(machines, *machine.DeepCopy())

		if sameRegistryAuthPolicy(machine.Spec.RegistryAuth, policy) {
			// The Machine already carries the desired policy; nothing to patch.
			continue
		}

		desired := machine.DeepCopy()
		desired.Spec.RegistryAuth = policy

		// A typed read from the client drops TypeMeta, so restore the known
		// Machine GVK before the executor can tell the object apart from any
		// other cluster-scoped one.
		gvk := v1alpha3.GroupVersion.WithKind("Machine")
		base := component.ToUnstructured(&machine)
		base.SetGroupVersionKind(gvk)
		object := component.ToUnstructured(desired)
		object.SetGroupVersionKind(gvk)

		operations = append(operations, component.Operation{
			Kind:      component.OpMergePatch,
			Component: "gantry",
			Base:      base,
			Object:    object,
		})
	}

	return operations, machines, nil
}

// eligibleFlexNodes returns the names of the Linux AKS Flex nodes the registry-
// auth policy applies to, in sorted order.
func eligibleFlexNodes(ctx context.Context, env *component.Env) ([]string, error) {
	nodes := metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}}
	if err := env.Client.List(ctx, &nodes, client.MatchingLabels(RegistryAuthNodeSelector())); err != nil {
		return nil, fmt.Errorf("read cached Linux Flex membership: %w", err)
	}

	names := make([]string, 0, len(nodes.Items))
	for i := range nodes.Items {
		if registryAuthEligibleNode(&nodes.Items[i]) {
			names = append(names, nodes.Items[i].Name)
		}
	}

	sort.Strings(names)

	return names, nil
}

// sameRegistryAuthPolicy reports whether the Machine's current registry-auth
// policy equals the desired one (both nil counts as a match).
func sameRegistryAuthPolicy(a, b *v1alpha3.RegistryAuthPolicy) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	return *a == *b
}

// registryAuthResult reads back the node-reported RegistryAuth conditions on the
// Machines and folds them into a component Result. Ready means every Machine
// reports the expected healthy condition for the current policy. A single
// failing node is reported as NotReady with RegistryAuthFailed; a node still
// installing (no condition yet) is reported as NotReady with
// RegistryAuthInstalling. The aggregation is bounded to the node count: it does
// not fan out per-node events on the control plane.
func registryAuthResult(machines []v1alpha3.Machine, active bool) *component.Result {
	if len(machines) == 0 {
		message := "no Linux AKS Flex nodes currently require registry-auth reconciliation; private pulls are unverified"
		if active {
			message = "managed ACR authentication is enabled but no agent-backed Flex nodes are present"
		}

		return &component.Result{Ready: true, Reason: component.ReasonReconciled, Message: message}
	}

	installing := 0
	var failures []string

	for i := range machines {
		cond := registryAuthCondition(&machines[i])
		switch {
		case cond == nil:
			installing++
		case cond.Status == metav1.ConditionTrue:
			// Healthy and verified by the agent.
		default:
			failures = append(failures, fmt.Sprintf("%s: %s (%s)", machines[i].Name, cond.Reason, cond.Message))
		}
	}

	if len(failures) > 0 {
		res := component.NotReady("RegistryAuthFailed", fmt.Sprintf("%d node(s) failed to configure the kubelet credential provider; first: %s", len(failures), failures[0]))

		return &res
	}

	if installing > 0 {
		res := component.NotReady("RegistryAuthInstalling", fmt.Sprintf("%d node(s) are still reporting their registry-auth state", installing))

		return &res
	}

	if active {
		return &component.Result{Ready: true, Reason: component.ReasonReconciled, Message: fmt.Sprintf("managed ACR credential provider configured and verified on %d node(s); private pulls and Gantry acceleration remain unverified", len(machines))}
	}

	return &component.Result{Ready: true, Reason: component.ReasonReconciled, Message: fmt.Sprintf("operator-owned node authentication removed on %d node(s); registry authentication is externally managed", len(machines))}
}

// registryAuthCondition returns the node-reported RegistryAuth condition on a
// Machine, or nil when it has not been reported yet.
func registryAuthCondition(machine *v1alpha3.Machine) *metav1.Condition {
	for i := range machine.Status.Conditions {
		if machine.Status.Conditions[i].Type == v1alpha3.MachineConditionRegistryAuth {
			return &machine.Status.Conditions[i]
		}
	}

	return apimeta.FindStatusCondition(machine.Status.Conditions, v1alpha3.MachineConditionRegistryAuth)
}

// planRetireLegacyRegistryAuth deletes the operator-owned objects from the old
// per-node installer: the gantry-acr-auth DaemonSet and ServiceAccount, the
// legacy ConfigMap migration marker, and the marker's Role/RoleBinding/Lease.
// Each object is deleted only when the operator owns it, so an unrelated object
// at the same name is never touched. Absence of any object is success.
func planRetireLegacyRegistryAuth(ctx context.Context, env *component.Env) ([]component.Operation, error) {
	objects := []client.Object{
		&appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: appsv1.SchemeGroupVersion.String(), Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: coordinationv1.SchemeGroupVersion.String(), Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: "gantry-acr-auth-restart", Namespace: env.Namespace}},
		&corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
	}

	var (
		operations   []component.Operation
		dependencies []component.ObjectRef
	)

	for _, object := range objects {
		gvk := object.GetObjectKind().GroupVersionKind()
		if err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(object), object); apierrors.IsNotFound(err) {
			continue
		} else if err != nil {
			return nil, err
		}

		if !registryAuthOwned(object) {
			continue
		}

		object.GetObjectKind().SetGroupVersionKind(gvk)
		op := component.DeleteOperation(object, "gantry", "")
		op.DependsOn = append(op.DependsOn, dependencies...)
		operations = append(operations, op)
		dependencies = append(dependencies, op.Ref())
	}

	return operations, nil
}

func registryAuthOwned(object client.Object) bool {
	return object.GetLabels()["app.kubernetes.io/name"] == registryAuthName &&
		object.GetLabels()["app.kubernetes.io/managed-by"] == installationManagerOperator
}
