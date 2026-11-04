// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

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
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const (
	registryAuthName         = "gantry-acr-auth"
	registryAuthHash         = "unbounded-cloud.io/registry-auth-config-hash"
	registryAuthAudienceName = "gantry-acr-kubelet-token-audience"
)

func registryAuthWorkloadEvents(env *component.Env, match func(client.Object) bool) predicate.Predicate {
	base := env.ManagedWorkloadPredicate(match)

	return predicate.Funcs{
		CreateFunc: base.Create,
		DeleteFunc: base.Delete,
		UpdateFunc: func(ev event.UpdateEvent) bool {
			if base.Update(ev) {
				return true
			}

			oldDS, oldOK := ev.ObjectOld.(*appsv1.DaemonSet)
			newDS, newOK := ev.ObjectNew.(*appsv1.DaemonSet)

			return oldOK && newOK && match(newDS) && newDS.Name == registryAuthName &&
				registryAuthWorkloadReady(oldDS) != registryAuthWorkloadReady(newDS)
		},
	}
}

func registryAuthWorkloadReady(ds *appsv1.DaemonSet) bool {
	return ds.Status.ObservedGeneration >= ds.Generation &&
		ds.Status.UpdatedNumberScheduled == ds.Status.DesiredNumberScheduled &&
		ds.Status.NumberAvailable == ds.Status.DesiredNumberScheduled
}

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

	var existing appsv1.DaemonSet

	err = env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	var retained corev1.ConfigMap
	if err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &retained); err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	var account corev1.ServiceAccount
	if !active && !registryAuthOwned(&existing) && !registryAuthOwned(&retained) {
		if err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &account); err != nil && !apierrors.IsNotFound(err) {
			return nil, nil, err
		}

		if !registryAuthOwned(&account) {
			return nil, nil, nil
		}
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}

	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	objects, workload := registryAuthObjects(env, payload, digest)

	var operations []component.Operation

	var dependencies []component.ObjectRef

	workloadMatches := false

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

			comparable := component.ToUnstructured(object)
			delete(comparable.Object, "status")

			if metadata, ok := comparable.Object["metadata"].(map[string]any); ok {
				delete(metadata, "creationTimestamp")
			}

			if component.DesiredFieldsMatch(comparable.Object, current.Object) {
				if object == workload {
					workloadMatches = true
				}

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

	result := component.NotReady("RegistryAuthInstalling", "installing the stock ACR credential provider on opted-in Flex nodes; inspect DaemonSet/"+registryAuthName+" and its pod logs")

	if workloadMatches && existing.Spec.Template.Annotations[registryAuthHash] == digest && registryAuthWorkloadReady(&existing) &&
		len(existing.Spec.Template.Spec.Containers) > 0 && existing.Spec.Template.Spec.Containers[0].Image == env.Config.Image("unbounded-operator") {
		message := "stock ACR credential provider configured; private pulls and Gantry acceleration remain unverified"
		if !active {
			message = "operator-owned node authentication removed; registry authentication is externally managed"
		}

		if existing.Status.DesiredNumberScheduled == 0 {
			message = "no Linux AKS Flex nodes currently require registry-auth reconciliation; private pulls are unverified"
		}

		result = component.Reconciled()
		result.Message = message

		if registryAuthOwned(&retained) {
			cleanup, err := planLegacyRegistryAuthCleanup(ctx, env, &retained)
			if err != nil {
				return nil, nil, err
			}

			operations = append(operations, cleanup...)
		}
	}

	return operations, &result, nil
}

// The old ConfigMap is the migration marker, deleted only after the old helpers
// have been replaced and their Lease permissions have been removed.
func planLegacyRegistryAuthCleanup(ctx context.Context, env *component.Env, marker *corev1.ConfigMap) ([]component.Operation, error) {
	objects := []client.Object{
		&rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace}},
		&coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: coordinationv1.SchemeGroupVersion.String(), Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: "gantry-acr-auth-restart", Namespace: env.Namespace}},
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

		if registryAuthOwned(object) {
			object.GetObjectKind().SetGroupVersionKind(gvk)
			op := component.DeleteOperation(object, "gantry", "")
			operations = append(operations, op)
			dependencies = append(dependencies, op.Ref())
		}
	}

	marker.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
	op := component.DeleteOperation(marker, "gantry", "")
	op.DependsOn = dependencies

	return append(operations, op), nil
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
	account := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata(), AutomountServiceAccountToken: ptr.To(false)}
	workload := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metadata(), Spec: appsv1.DaemonSetSpec{
		Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": registryAuthName}},
		UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromString("100%"))}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{registryAuthHash: hash}}, Spec: corev1.PodSpec{
			NodeSelector:       map[string]string{"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue},
			Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			ServiceAccountName: registryAuthName, HostPID: true, HostNetwork: true, DNSPolicy: corev1.DNSClusterFirstWithHostNet,
			AutomountServiceAccountToken: ptr.To(false),
			Containers: []corev1.Container{{
				Name: "configure", Image: env.Config.Image("unbounded-operator"), ImagePullPolicy: corev1.PullIfNotPresent,
				Args: []string{"registry-auth-node"},
				// Kubelet expands $(VAR) in literal env values; $$ preserves policy dollars.
				Env:             []corev1.EnvVar{{Name: "REGISTRY_AUTH_CONFIG", Value: strings.ReplaceAll(string(payload), "$", "$$")}},
				SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_CHROOT"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Ports:           []corev1.ContainerPort{{Name: "health", ContainerPort: 9096}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9096)}}, PeriodSeconds: 5},
				LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/livez", Port: intstr.FromInt32(9096)}}, InitialDelaySeconds: 10, PeriodSeconds: 10},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: ptr.To(corev1.HostPathDirectory)}}},
			},
		}},
	}}

	return []client.Object{account, workload}, workload
}
