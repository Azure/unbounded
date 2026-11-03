// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const (
	registryAuthName         = "gantry-acr-auth"
	registryAuthHash         = "unbounded-cloud.io/registry-auth-config-hash"
	registryAuthAudienceName = "gantry-acr-kubelet-token-audience"
)

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

	audienceOperations, err := planRegistryAuthAudience(ctx, env, cfg)
	if err != nil {
		return nil, nil, err
	}

	var existing appsv1.DaemonSet

	err = env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	var retained corev1.ConfigMap
	if err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &retained); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	if !active && !registryAuthOwned(&existing) && !registryAuthOwned(&retained) {
		return audienceOperations, nil, nil
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}

	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	objects, workload := registryAuthObjects(env, payload, digest)

	operations := append([]component.Operation(nil), audienceOperations...)

	if !active {
		var binding rbacv1.ClusterRoleBinding
		if err := env.LiveReader().Get(ctx, client.ObjectKey{Name: registryauth.IdentityRoleName}, &binding); err == nil {
			if binding.Labels["app.kubernetes.io/managed-by"] != "unbounded-acr-attach" || binding.Labels["unbounded-cloud.io/namespace"] != env.Namespace {
				return nil, nil, fmt.Errorf("refusing to remove an unowned identity-binding ClusterRoleBinding/%s", binding.Name)
			}

			binding.TypeMeta = metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}
			operations = append(operations, component.Operation{Kind: component.OpDelete, Object: component.ToUnstructured(&binding), Component: "gantry"})
		} else if !apierrors.IsNotFound(err) {
			return nil, nil, err
		}
	}

	var dependencies []component.ObjectRef
	for _, op := range audienceOperations {
		dependencies = append(dependencies, op.Ref())
	}

	for _, object := range objects {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GetObjectKind().GroupVersionKind())

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(object), current)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, err
		}

		// A concurrent create must not turn a missing object into a forced adoption.
		kind := component.OpCreateIfAbsent

		if err == nil {
			if !registryAuthOwned(current) {
				return nil, nil, fmt.Errorf("refusing to overwrite an unowned registry-auth %s/%s", current.GetKind(), current.GetName())
			}

			if object.GetName() == registryauth.RestartLeaseName {
				continue
			}

			kind = component.OpApply

			object.SetUID(current.GetUID())
			object.SetResourceVersion(current.GetResourceVersion())
		}

		op := component.Operation{Kind: kind, Object: component.ToUnstructured(object), Component: "gantry"}
		if object == workload {
			op.DependsOn = append(op.DependsOn, dependencies...)
		} else {
			dependencies = append(dependencies, op.Ref())
		}

		operations = append(operations, op)
	}

	result := component.NotReadyAfter("RegistryAuthInstalling", "installing the stock ACR credential provider on opted-in Flex nodes; inspect DaemonSet/"+registryAuthName+" and its pod logs", 10*time.Second)

	if existing.Spec.Template.Annotations[registryAuthHash] == digest && existing.Status.ObservedGeneration >= existing.Generation &&
		existing.Status.UpdatedNumberScheduled == existing.Status.DesiredNumberScheduled && existing.Status.NumberAvailable == existing.Status.DesiredNumberScheduled &&
		len(existing.Spec.Template.Spec.Containers) > 0 && existing.Spec.Template.Spec.Containers[0].Image == env.Config.Image("unbounded-operator") {
		message := "stock ACR credential provider configured; private pulls and Gantry acceleration remain unverified"
		if !active {
			message = "operator-owned node authentication removed; registry authentication is externally managed"
		}

		if existing.Status.DesiredNumberScheduled == 0 {
			message = "no Linux AKS Flex nodes currently require registry-auth reconciliation; private pulls are unverified"
		}

		result = component.ReconciledAfter(message, 30*time.Second)
	}

	return operations, &result, nil
}

func planRegistryAuthAudience(ctx context.Context, env *component.Env, cfg registryauth.Config) ([]component.Operation, error) {
	active := cfg.Mode == registryauth.IdentityBindingMode
	metadata := metav1.ObjectMeta{
		Name: registryAuthAudienceName,
		Labels: map[string]string{
			"app.kubernetes.io/name":       registryAuthName,
			"app.kubernetes.io/managed-by": installationManagerOperator,
		},
	}
	role := &rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"},
		ObjectMeta: metadata,
	}
	binding := &rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"},
		ObjectMeta: metadata,
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: role.Name},
	}

	if active {
		var nodes corev1.NodeList
		if err := env.LiveReader().List(ctx, &nodes, client.MatchingLabels{
			"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue,
		}); err != nil {
			return nil, fmt.Errorf("list Linux Flex nodes for credential-provider audience authorization: %w", err)
		}

		sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })

		for _, node := range nodes.Items {
			if node.DeletionTimestamp == nil {
				binding.Subjects = append(binding.Subjects, rbacv1.Subject{
					Kind: rbacv1.UserKind, APIGroup: rbacv1.GroupName, Name: "system:node:" + node.Name,
				})
			}
		}

		// NodeRestriction separately checks that the token is bound to a Pod on the requesting node.
		role.Rules = []rbacv1.PolicyRule{{
			APIGroups: []string{""}, Resources: []string{cfg.Audience},
			Verbs: []string{"request-serviceaccounts-token-audience"},
		}}
	}

	var operations []component.Operation

	for _, object := range []client.Object{role, binding} {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(object.GetObjectKind().GroupVersionKind())

		err := env.LiveReader().Get(ctx, client.ObjectKeyFromObject(object), current)
		if err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}

		kind := component.OpCreateIfAbsent

		if err == nil {
			if !registryAuthOwned(current) {
				if !active {
					continue
				}

				return nil, fmt.Errorf("refusing to overwrite an unowned registry-auth %s/%s", current.GetKind(), current.GetName())
			}

			kind = component.OpApply

			object.SetUID(current.GetUID())
			object.SetResourceVersion(current.GetResourceVersion())
		} else if !active {
			continue
		}

		if !active {
			kind = component.OpDelete
		}

		op := component.Operation{Kind: kind, Object: component.ToUnstructured(object), Component: "gantry"}
		if active && object == binding {
			op.DependsOn = []component.ObjectRef{operations[0].Ref()}
		}

		operations = append(operations, op)
	}

	return operations, nil
}

func registryAuthOwned(object client.Object) bool {
	return object.GetLabels()["app.kubernetes.io/name"] == registryAuthName &&
		object.GetLabels()["app.kubernetes.io/managed-by"] == installationManagerOperator
}

func registryAuthObjects(env *component.Env, payload []byte, hash string) ([]client.Object, *appsv1.DaemonSet) {
	labels := map[string]string{"app.kubernetes.io/name": registryAuthName, "app.kubernetes.io/managed-by": installationManagerOperator}
	metadata := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace, Labels: labels}
	}
	config := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metadata(), Data: map[string]string{"config.json": string(payload)}}
	account := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata()}
	account.Name = registryAuthName
	role := &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"}, ObjectMeta: metadata(), Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{registryauth.RestartLeaseName}, Verbs: []string{"get", "update"}}}}
	binding := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"}, ObjectMeta: metadata(), RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: registryAuthName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: registryAuthName, Namespace: env.Namespace}}}
	lease := &coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: registryauth.RestartLeaseName, Namespace: env.Namespace, Labels: labels}}
	workload := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metadata(), Spec: appsv1.DaemonSetSpec{
		Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": registryAuthName}},
		UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1))}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{registryAuthHash: hash}}, Spec: corev1.PodSpec{
			NodeSelector:       map[string]string{"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue},
			Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			ServiceAccountName: registryAuthName, HostPID: true, HostNetwork: true, DNSPolicy: corev1.DNSClusterFirstWithHostNet,
			Containers: []corev1.Container{{
				Name: "configure", Image: env.Config.Image("unbounded-operator"), ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"registry-auth-node"},
				Env:             []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}, {Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
				SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_CHROOT"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Ports:           []corev1.ContainerPort{{Name: "health", ContainerPort: 9096}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9096)}}, PeriodSeconds: 5},
				LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/livez", Port: intstr.FromInt32(9096)}}, InitialDelaySeconds: 10, PeriodSeconds: 10},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}, {Name: "config", MountPath: "/etc/registry-auth", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{
				{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: ptr.To(corev1.HostPathDirectory)}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: registryAuthName}}}},
			},
		}},
	}}

	return []client.Object{config, account, role, binding, lease, workload}, workload
}
