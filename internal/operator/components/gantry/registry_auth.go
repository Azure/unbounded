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
		// Every Machine create is relevant: an external (nil-policy) Machine is
		// still agent-backed and reports its registry-auth readiness, so dropping
		// it here would lose the initial delivery and readiness for external
		// nodes.
		CreateFunc: func(ev event.CreateEvent) bool {
			_, ok := ev.Object.(*v1alpha3.Machine)

			return ok
		},
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(ev event.GenericEvent) bool { return false },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			oldMachine, okOld := ev.ObjectOld.(*v1alpha3.Machine)

			newMachine, okNew := ev.ObjectNew.(*v1alpha3.Machine)

			if !okOld || !okNew {
				return false
			}

			// The delivery plan indexes Machines by NodeName(), so a change to
			// spec.kubernetes.nodeRef.name (the association it reads) re-plans
			// which node a Machine is delivered to. Include it without
			// broadening to unrelated spec/status traffic.
			return !sameRegistryAuthPolicy(oldMachine.Spec.RegistryAuth, newMachine.Spec.RegistryAuth) ||
				oldMachine.NodeName() != newMachine.NodeName() ||
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

	// A change that only updates ObservedGeneration still reconciles: the node
	// re-acknowledged the current generation, which the readiness aggregation
	// depends on. The message is part of the contract too (terminal vs
	// transient reasons are carried there).
	return oldCond.Status != newCond.Status ||
		oldCond.Reason != newCond.Reason ||
		oldCond.ObservedGeneration != newCond.ObservedGeneration ||
		oldCond.Message != newCond.Message
}

const (
	registryAuthName         = "gantry-acr-auth"
	registryAuthHash         = "unbounded-cloud.io/registry-auth-config-hash"
	registryAuthAudienceName = "gantry-acr-kubelet-token-audience"
)

// registryAuthNodePredicate enqueues a gantry re-reconcile on Node membership
// changes for the Linux AKS Flex nodes, so the registry-auth plan reflects a
// node joining or leaving the fleet even before its Machine registers.
//
// The UpdateFunc fires only when eligibility CHANGES (a node becomes or stops
// being a Linux AKS Flex node), matching registryAuthNodeEvents. A status-only
// update (NodeReady flaps, heartbeats, unrelated label churn) on an already
// eligible node therefore does not enqueue the singleton fleet planner, which at
// 10k nodes would otherwise turn every heartbeat into a whole-fleet pass. The
// singleton request maps every event to one reconcile pass; the eligibility
// check bounds it to the nodes the policy applies to.
func registryAuthNodePredicate() predicate.Predicate {
	eligible := func(obj client.Object) bool {
		node, ok := obj.(*corev1.Node)

		return ok && registryAuthEligibleNode(node)
	}

	return predicate.Funcs{
		CreateFunc:  func(ev event.CreateEvent) bool { return eligible(ev.Object) },
		DeleteFunc:  func(ev event.DeleteEvent) bool { return eligible(ev.Object) },
		GenericFunc: func(ev event.GenericEvent) bool { return eligible(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			// Reconcile only when eligibility changes, so a status-only update
			// on an already-eligible node is not amplified into a fleet pass.
			return eligible(ev.ObjectOld) != eligible(ev.ObjectNew)
		},
	}
}

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

	return operations, registryAuthResult(machines, policy, active), nil
}

// planRegistryAuthMachines writes the policy to every eligible Flex node's
// Machine and returns the planned operations plus the observed Machines, so the
// caller can read back the node-reported readiness.
func planRegistryAuthMachines(ctx context.Context, env *component.Env, policy *v1alpha3.RegistryAuthPolicy, active bool) ([]component.Operation, []v1alpha3.Machine, error) {
	nodes, err := eligibleFlexNodes(ctx, env)
	if err != nil {
		return nil, nil, err
	}

	// Read the Machines once through the cached client (a single bounded List,
	// not a live read per node) and associate each node with its Machine by
	// NodeName. This is the 10k-node contract: observation is a single List of
	// the watched kind plus an in-memory map, not N apiserver calls.
	machinesList := &v1alpha3.MachineList{}
	if err := env.Client.List(ctx, machinesList); err != nil {
		return nil, nil, fmt.Errorf("list Machines for registry-auth: %w", err)
	}

	byNode := make(map[string]*v1alpha3.Machine, len(machinesList.Items))
	for i := range machinesList.Items {
		m := &machinesList.Items[i]
		byNode[m.NodeName()] = m
	}

	var (
		operations   []component.Operation
		machines     []v1alpha3.Machine
		pendingNodes []string
	)

	gvk := v1alpha3.GroupVersion.WithKind("Machine")

	for _, name := range nodes {
		machine, ok := byNode[name]
		if !ok {
			// A node without a Machine is not yet agent-backed. It is kept as
			// pending (reported by the readiness aggregation) rather than
			// silently skipped, so initial delivery and membership changes are
			// not lost.
			pendingNodes = append(pendingNodes, name)

			continue
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
		base := component.ToUnstructured(machine)
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

	// Pending nodes are reported through a synthetic Machine so the readiness
	// aggregation sees them as installing rather than ignoring them.
	for _, name := range pendingNodes {
		machines = append(machines, v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: name}})
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
// Machines and folds them into a component Result. Ready means, for every
// Machine, that (a) the desired policy has actually been delivered to
// Machine.spec.registryAuth (not merely planned as a patch), and (b) the agent
// reports the expected healthy condition for the current policy: Configured for
// a managed provider, External when authentication is left untouched, observed
// at or after the current generation. A single failing node is reported as
// NotReady with RegistryAuthFailed; a node still installing (no condition yet,
// an unacknowledged generation, or an undelivered policy) is reported as NotReady
// with RegistryAuthInstalling. The aggregation is bounded to the node count: it
// does not fan out per-node events on the control plane.
func registryAuthResult(machines []v1alpha3.Machine, policy *v1alpha3.RegistryAuthPolicy, active bool) *component.Result {
	if len(machines) == 0 {
		message := "no Linux AKS Flex nodes currently require registry-auth reconciliation; private pulls are unverified"
		if active {
			message = "managed ACR authentication is enabled but no agent-backed Flex nodes are present"
		}

		return &component.Result{Ready: true, Reason: component.ReasonReconciled, Message: message}
	}

	// The healthy reason the agent reports for each mode. A True condition with
	// a different reason means the node is verified for a different policy than
	// the one being delivered, which is not readiness for this intent.
	wantReason := registryauth.ConditionReasonExternal
	if active {
		wantReason = registryauth.ConditionReasonConfigured
	}

	installing := 0

	var failures []string

	for i := range machines {
		m := &machines[i]

		cond := registryAuthCondition(m)

		switch {
		case cond == nil:
			// No condition reported yet: the node is still installing.
			installing++
		case cond.Status == metav1.ConditionTrue &&
			cond.Reason == wantReason &&
			cond.ObservedGeneration >= m.Generation &&
			sameRegistryAuthPolicy(m.Spec.RegistryAuth, policy):
			// Healthy, verified by the agent, observed the current generation,
			// for the exact policy being delivered, with the policy present on
			// the spec (not merely planned as a patch).
		case cond.Status == metav1.ConditionTrue:
			// The condition is healthy but either observed before the current
			// generation, reports a different reason, or the desired policy is
			// not yet on the spec; the node has not acknowledged this policy yet.
			installing++
		case cond.Reason == registryauth.ConditionReasonRestartBudgetExhausted && cond.ObservedGeneration >= m.Generation:
			// A current-generation budget exhaustion is a genuine failure: no
			// retry is left for the policy being delivered.
			failures = append(failures, fmt.Sprintf("%s: %s (%s)", m.Name, cond.Reason, cond.Message))
		default:
			// A False condition that is not a current-generation budget
			// exhaustion is the agent scheduling a bounded retry (backoff,
			// apply, verify, ledger, budget-reset) or a stale verdict from a
			// prior generation; the node is still converging, not failed.
			installing++
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
