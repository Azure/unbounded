// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const registryAuthBindingSize = 1000

// RegistryAuthNodeSelector restricts the central informer on the server, not
// merely the events delivered to the controller.
func RegistryAuthNodeSelector() map[string]string {
	return map[string]string{"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue}
}

// RegistryAuthAudienceReconciler maintains named-node grants independently of
// Site and Gantry workload reconciliation.
type RegistryAuthAudienceReconciler struct {
	Client    client.Client
	APIReader client.Reader
	Namespace string
}

func (r *RegistryAuthAudienceReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	cfg := registryauth.Config{Mode: registryauth.ExternalMode}

	var source corev1.ConfigMap
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: r.Namespace, Name: configName}, &source); err == nil {
		var err error

		cfg, err = registryauth.Derive(source.Data["config.yaml"], source.Data[registryauth.ConfigKey])
		if err != nil {
			return ctrl.Result{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	env := &component.Env{Client: r.Client, APIReader: r.APIReader, Namespace: r.Namespace}

	operations, err := planRegistryAuthAudience(ctx, env, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cfg.Mode == registryauth.IdentityBindingMode {
		identityOperations, err := planRegistryAuthIdentityUse(ctx, env, cfg)
		if err != nil {
			return ctrl.Result{}, err
		}

		operations = append(operations, identityOperations...)
	}

	for _, op := range operations {
		// Typed creates/deletes reuse the typed RBAC informers, including on
		// create races; no unstructured informer is started as a side effect.
		var object client.Object

		switch op.Object.GetKind() {
		case "ClusterRole":
			object = &rbacv1.ClusterRole{}
		case "ClusterRoleBinding":
			object = &rbacv1.ClusterRoleBinding{}
		default:
			return ctrl.Result{}, fmt.Errorf("unsupported audience operation %s", op.Ref())
		}

		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(op.Object.Object, object); err != nil {
			return ctrl.Result{}, err
		}

		switch op.Kind {
		case component.OpCreateIfAbsent:
			err = r.Client.Create(ctx, object)
		case component.OpApply:
			err = r.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(op.Object), client.FieldOwner(component.FieldOwner), client.ForceOwnership)
		case component.OpDelete:
			err = r.Client.Delete(ctx, object, client.Preconditions{UID: ptr.To(object.GetUID()), ResourceVersion: ptr.To(object.GetResourceVersion())})
		}

		if err != nil {
			return ctrl.Result{}, fmt.Errorf("%s: %w", op.Ref(), err)
		}
	}

	return ctrl.Result{}, nil
}

func (r *RegistryAuthAudienceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	env := &component.Env{Namespace: r.Namespace}

	return ctrl.NewControllerManagedBy(mgr).
		Named("gantry-registry-auth-audience").
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Watches(&corev1.ConfigMap{}, registryAuthAudienceEvents(),
			builder.WithPredicates(env.ManagedConfigPredicate(env.InNamespaceNamed(configName)))).
		WatchesMetadata(&corev1.Node{}, registryAuthAudienceEvents(), builder.WithPredicates(registryAuthNodeEvents())).
		Watches(&rbacv1.ClusterRole{}, registryAuthAudienceEvents(), builder.WithPredicates(registryAuthRBACEvents())).
		Watches(&rbacv1.ClusterRoleBinding{}, registryAuthAudienceEvents(), builder.WithPredicates(registryAuthRBACEvents())).
		Complete(r)
}

func registryAuthRBACEvents() predicate.Predicate {
	match := func(object client.Object) bool {
		switch object.(type) {
		case *rbacv1.ClusterRole:
			return object.GetName() == registryAuthAudienceName || object.GetName() == registryauth.IdentityRoleName
		case *rbacv1.ClusterRoleBinding:
			return registryAuthBindingName(object.GetName()) || object.GetName() == registryauth.IdentityRoleName
		default:
			return false
		}
	}

	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return match(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return match(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			if !match(ev.ObjectNew) {
				return false
			}

			if registryAuthOwned(ev.ObjectOld) != registryAuthOwned(ev.ObjectNew) ||
				ev.ObjectOld.GetLabels()["app.kubernetes.io/managed-by"] != ev.ObjectNew.GetLabels()["app.kubernetes.io/managed-by"] ||
				ev.ObjectOld.GetLabels()["unbounded-cloud.io/namespace"] != ev.ObjectNew.GetLabels()["unbounded-cloud.io/namespace"] {
				return true
			}

			switch newer := ev.ObjectNew.(type) {
			case *rbacv1.ClusterRole:
				older, ok := ev.ObjectOld.(*rbacv1.ClusterRole)

				return !ok || !apiequality.Semantic.DeepEqual(older.Rules, newer.Rules) ||
					!apiequality.Semantic.DeepEqual(older.AggregationRule, newer.AggregationRule)
			case *rbacv1.ClusterRoleBinding:
				older, ok := ev.ObjectOld.(*rbacv1.ClusterRoleBinding)

				return !ok || older.RoleRef != newer.RoleRef || !slices.Equal(older.Subjects, newer.Subjects) ||
					older.Annotations[registryAuthHash] != newer.Annotations[registryAuthHash] ||
					older.Labels["app.kubernetes.io/managed-by"] != newer.Labels["app.kubernetes.io/managed-by"] ||
					older.Labels["unbounded-cloud.io/namespace"] != newer.Labels["unbounded-cloud.io/namespace"]
			}

			return false
		},
	}
}

func registryAuthAudienceEvents() handler.EventHandler {
	enqueue := func(q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
		// AddAfter coalesces a burst into one singleton without polling when idle.
		q.AddAfter(ctrl.Request{}, 250*time.Millisecond)
	}

	return handler.Funcs{
		CreateFunc: func(_ context.Context, _ event.CreateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q)
		},
		UpdateFunc: func(_ context.Context, _ event.UpdateEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q)
		},
		DeleteFunc: func(_ context.Context, _ event.DeleteEvent, q workqueue.TypedRateLimitingInterface[ctrl.Request]) {
			enqueue(q)
		},
	}
}

func registryAuthNodeEvents() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(ev event.CreateEvent) bool { return registryAuthEligibleNode(ev.Object) },
		DeleteFunc: func(ev event.DeleteEvent) bool { return registryAuthEligibleNode(ev.Object) },
		UpdateFunc: func(ev event.UpdateEvent) bool {
			return registryAuthEligibleNode(ev.ObjectOld) != registryAuthEligibleNode(ev.ObjectNew)
		},
	}
}

func registryAuthEligibleNode(object client.Object) bool {
	return object.GetDeletionTimestamp() == nil && object.GetLabels()["kubernetes.io/os"] == "linux" &&
		object.GetLabels()[registryauth.FlexNodeLabel] == registryauth.FlexNodeValue
}

func registryAuthBindingName(name string) bool {
	return name == registryAuthAudienceName || strings.HasPrefix(name, registryAuthAudienceName+"-")
}

func planRegistryAuthAudience(ctx context.Context, env *component.Env, cfg registryauth.Config) ([]component.Operation, error) {
	active := cfg.Mode == registryauth.IdentityBindingMode
	metadata := metav1.ObjectMeta{Name: registryAuthAudienceName, Labels: map[string]string{
		"app.kubernetes.io/name": registryAuthName, "app.kubernetes.io/managed-by": installationManagerOperator,
	}}
	role := &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata,
	}

	var observed rbacv1.ClusterRoleBindingList
	if err := env.Client.List(ctx, &observed); err != nil {
		return nil, err
	}

	bindings := map[string]*rbacv1.ClusterRoleBinding{}

	for i := range observed.Items {
		binding := &observed.Items[i]
		if registryAuthBindingName(binding.Name) {
			bindings[binding.Name] = binding
		}
	}

	names := []string{registryAuthAudienceName}
	for name := range bindings {
		if name != registryAuthAudienceName {
			names = append(names, name)
		}
	}

	sort.Strings(names)

	desired := map[string][]rbacv1.Subject{}

	if active {
		// NodeRestriction independently enforces Pod/node binding.
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{cfg.Audience},
			Verbs: []string{"request-serviceaccounts-token-audience"},
		}}

		nodes := metav1.PartialObjectMetadataList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"}}
		if err := env.Client.List(ctx, &nodes, client.MatchingLabels(RegistryAuthNodeSelector())); err != nil {
			return nil, fmt.Errorf("read cached Linux Flex membership: %w", err)
		}

		eligible := map[string]bool{}

		for i := range nodes.Items {
			if registryAuthEligibleNode(&nodes.Items[i]) {
				eligible["system:node:"+nodes.Items[i].Name] = true
			}
		}

		assigned := map[string]bool{}

		for _, name := range names {
			if existing := bindings[name]; existing != nil {
				if !registryAuthOwned(existing) {
					return nil, fmt.Errorf("refusing to overwrite an unowned registry-auth ClusterRoleBinding/%s", name)
				}

				for _, subject := range existing.Subjects {
					if subject.Kind == rbacv1.UserKind && subject.APIGroup == rbacv1.GroupName && subject.Namespace == "" &&
						eligible[subject.Name] && !assigned[subject.Name] && len(desired[name]) < registryAuthBindingSize {
						desired[name] = append(desired[name], subject)
						assigned[subject.Name] = true
					}
				}
			}
		}

		var pending []string

		for name := range eligible {
			if !assigned[name] {
				pending = append(pending, name)
			}
		}

		sort.Strings(pending)

		index, nextShard := 0, 1
		for _, node := range pending {
			for index < len(names) && len(desired[names[index]]) == registryAuthBindingSize {
				index++
			}

			if index == len(names) {
				var name string
				for {
					name = fmt.Sprintf("%s-%04d", registryAuthAudienceName, nextShard)
					nextShard++

					if _, exists := bindings[name]; !exists {
						break
					}
				}

				names = append(names, name)
				bindings[name] = nil
			}

			name := names[index]
			desired[name] = append(desired[name], rbacv1.Subject{Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: node})
		}
	}

	var operations []component.Operation

	roleOp, err := registryAuthAudienceOperation(ctx, env, role, active)
	if err != nil {
		return nil, err
	}

	if active && roleOp != nil {
		if roleOp.Kind == component.OpDelete {
			return []component.Operation{*roleOp}, nil
		}

		operations = append(operations, *roleOp)
	}
	// Populate new shards before truncating the legacy single binding.
	names = append(names[1:], names[0])
	for _, name := range names {
		binding := &rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"},
			ObjectMeta: *metadata.DeepCopy(),
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
			Subjects:   desired[name],
		}
		binding.Name = name
		sort.Slice(binding.Subjects, func(i, j int) bool { return binding.Subjects[i].Name < binding.Subjects[j].Name })

		op, err := registryAuthAudienceOperation(ctx, env, binding, active)
		if err != nil {
			return nil, err
		}

		if op != nil {
			if active && op.Kind == component.OpDelete {
				// Repair an immutable wrong RoleRef before moving legacy grants.
				return append(operations, *op), nil
			}

			if active && roleOp != nil {
				op.DependsOn = []component.ObjectRef{roleOp.Ref()}
			}

			if active && name == registryAuthAudienceName {
				for _, earlier := range operations {
					if earlier.Object.GetKind() == "ClusterRoleBinding" {
						op.DependsOn = append(op.DependsOn, earlier.Ref())
					}
				}
			}

			operations = append(operations, *op)
		}
	}

	if !active {
		if roleOp != nil {
			operations = append(operations, *roleOp)
		}

		binding := &rbacv1.ClusterRoleBinding{
			TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"},
			ObjectMeta: metav1.ObjectMeta{Name: registryauth.IdentityRoleName},
		}

		op, err := registryAuthAudienceOperation(ctx, env, binding, false)
		if err != nil {
			return nil, err
		}

		if op != nil {
			operations = append(operations, *op)
		}
	}

	return operations, nil
}

func planRegistryAuthIdentityUse(ctx context.Context, env *component.Env, cfg registryauth.Config) ([]component.Operation, error) {
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}

	hash := sha256.Sum256(payload)
	metadata := metav1.ObjectMeta{Name: registryauth.IdentityRoleName, Labels: map[string]string{
		"app.kubernetes.io/managed-by": "unbounded-acr-attach", "unbounded-cloud.io/namespace": env.Namespace,
	}}
	role := &rbacv1.ClusterRole{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata,
		Rules: []rbacv1.PolicyRule{{APIGroups: []string{"cid.wi.aks.azure.com"}, Resources: []string{cfg.ClientID}, Verbs: []string{"use-managed-identity"}}},
	}
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "system:serviceaccounts"}},
	}
	binding.Annotations = map[string]string{registryAuthHash: hex.EncodeToString(hash[:])}

	var operations []component.Operation

	for _, object := range []client.Object{role, binding} {
		op, err := registryAuthAudienceOperation(ctx, env, object, true)
		if err != nil {
			return nil, err
		}

		if op != nil {
			operations = append(operations, *op)
			if op.Kind == component.OpDelete {
				break
			}
		}
	}

	return operations, nil
}

func registryAuthRBACOwned(object client.Object, namespace string) bool {
	if object.GetName() == registryauth.IdentityRoleName {
		return object.GetLabels()["app.kubernetes.io/managed-by"] == "unbounded-acr-attach" &&
			object.GetLabels()["unbounded-cloud.io/namespace"] == namespace
	}

	return registryAuthOwned(object)
}

func registryAuthAudienceOperation(ctx context.Context, env *component.Env, desired client.Object, active bool) (*component.Operation, error) {
	current, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return nil, fmt.Errorf("unsupported registry-auth object %T", desired)
	}

	err := env.Client.Get(ctx, client.ObjectKeyFromObject(desired), current)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}

	kind := component.OpCreateIfAbsent

	if err == nil {
		if !registryAuthRBACOwned(current, env.Namespace) {
			if !active && desired.GetName() != registryauth.IdentityRoleName {
				return nil, nil
			}

			return nil, fmt.Errorf("refusing to overwrite an unowned registry-auth %s/%s", desired.GetObjectKind().GroupVersionKind().Kind, desired.GetName())
		}

		equal := false
		replace := false

		switch wanted := desired.(type) {
		case *rbacv1.ClusterRoleBinding:
			existing, bindingOK := current.(*rbacv1.ClusterRoleBinding)
			if !bindingOK {
				return nil, fmt.Errorf("unexpected registry-auth binding %T", current)
			}

			equal = wanted.RoleRef == existing.RoleRef && slices.Equal(wanted.Subjects, existing.Subjects) &&
				(wanted.Name != registryauth.IdentityRoleName || wanted.Annotations[registryAuthHash] == existing.Annotations[registryAuthHash])
			replace = wanted.RoleRef != existing.RoleRef
		case *rbacv1.ClusterRole:
			existing, roleOK := current.(*rbacv1.ClusterRole)
			if !roleOK {
				return nil, fmt.Errorf("unexpected registry-auth role %T", current)
			}

			equal = existing.AggregationRule == nil && apiequality.Semantic.DeepEqual(wanted.Rules, existing.Rules)
			replace = existing.AggregationRule != nil
		}

		if active && equal {
			return nil, nil
		}

		kind = component.OpApply
		if !active || replace {
			kind = component.OpDelete
		}
	} else if !active {
		return nil, nil
	}
	// Only changed resources need an authoritative ownership/concurrency read.
	live, ok := desired.DeepCopyObject().(client.Object)
	if !ok {
		return nil, fmt.Errorf("unsupported registry-auth object %T", desired)
	}

	liveErr := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(desired), live)
	if liveErr != nil && !apierrors.IsNotFound(liveErr) {
		return nil, liveErr
	}

	if liveErr == nil && !registryAuthRBACOwned(live, env.Namespace) {
		return nil, fmt.Errorf("refusing to overwrite an unowned registry-auth %s/%s", desired.GetObjectKind().GroupVersionKind().Kind, desired.GetName())
	}

	if (err == nil) != (liveErr == nil) || (err == nil && (current.GetUID() != live.GetUID() || current.GetResourceVersion() != live.GetResourceVersion())) {
		return nil, fmt.Errorf("registry-auth cache changed for %s; retrying", desired.GetName())
	}

	if err == nil {
		desired.SetUID(current.GetUID())
		desired.SetResourceVersion(current.GetResourceVersion())
	}

	object := component.ToUnstructured(desired)
	if binding, ok := desired.(*rbacv1.ClusterRoleBinding); ok && len(binding.Subjects) == 0 {
		object.Object["subjects"] = []any{}
	}

	return &component.Operation{Kind: kind, Object: object, Component: "gantry"}, nil
}
