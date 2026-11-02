// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	gantrymanifests "github.com/Azure/unbounded/deploy/gantry"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/registryauth"
)

const (
	registryAuthName = "gantry-acr-auth"
	registryAuthHash = "unbounded-cloud.io/registry-auth-config-hash"
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

	active := cfg.Mode != "external" && len(cfg.Registries) > 0

	var existing appsv1.DaemonSet

	err = env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, nil, err
	}

	if !active && apierrors.IsNotFound(err) {
		// The generated config records an installation even if someone deleted
		// its helper DaemonSet before owned-file cleanup finished.
		var retained corev1.ConfigMap
		if err := env.LiveReader().Get(ctx, client.ObjectKey{Namespace: env.Namespace, Name: registryAuthName}, &retained); apierrors.IsNotFound(err) {
			return nil, nil, nil
		} else if err != nil {
			return nil, nil, err
		}
	}

	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, nil, err
	}

	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	objects, workload := registryAuthObjects(env, payload, digest)
	operations := make([]component.Operation, 0, len(objects))

	var dependencies []component.ObjectRef

	for _, object := range objects {
		op := component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(object), Component: "gantry"}
		if object.GetName() == registryauth.RestartLeaseName {
			op.Kind = component.OpCreateIfAbsent
		}

		if object == workload {
			op.DependsOn = append(op.DependsOn, dependencies...)
		} else {
			dependencies = append(dependencies, op.Ref())
		}

		operations = append(operations, op)
	}

	result := component.NotReadyAfter("RegistryAuthInstalling", "reconciling federated ACR brokers on Flex nodes; inspect DaemonSet/"+registryAuthName+" and its pod logs for federation or registry-permission errors", 10*time.Second)

	if existing.Spec.Template.Annotations[registryAuthHash] == digest && existing.Status.ObservedGeneration >= existing.Generation &&
		existing.Status.UpdatedNumberScheduled == existing.Status.DesiredNumberScheduled && existing.Status.NumberAvailable == existing.Status.DesiredNumberScheduled &&
		len(existing.Spec.Template.Spec.Containers) > 0 && existing.Spec.Template.Spec.Containers[0].Image == env.Config.RegistryAuthNodeImage() {
		message := "federated node-wide ACR broker installed; repository pull permission is unverified (set node-auth.yaml probeImage)"
		if !active {
			message = "operator-owned node authentication removed; registry authentication is externally managed"
		} else if cfg.ProbeImage != "" {
			message = "federated node-wide ACR broker installed; private-image authorization verified on Flex nodes"
		}

		if existing.Status.DesiredNumberScheduled == 0 {
			message = "no Linux AKS Flex nodes currently require registry-auth reconciliation; private pulls are unverified"
		}

		result = component.ReconciledAfter(message, 30*time.Second)
	}

	return operations, &result, nil
}

func registryAuthObjects(env *component.Env, payload []byte, hash string) ([]client.Object, *appsv1.DaemonSet) {
	labels := map[string]string{"app.kubernetes.io/name": registryAuthName, "app.kubernetes.io/managed-by": "unbounded-operator"}
	metadata := func() metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: registryAuthName, Namespace: env.Namespace, Labels: labels}
	}
	config := &corev1.ConfigMap{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}, ObjectMeta: metadata(), Data: map[string]string{"config.json": string(payload)}}
	account := &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata()}
	account.Name = registryauth.ServiceAccountName
	role := &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"}, ObjectMeta: metadata(), Rules: []rbacv1.PolicyRule{{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, ResourceNames: []string{registryauth.RestartLeaseName}, Verbs: []string{"get", "update"}}}}
	binding := &rbacv1.RoleBinding{TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"}, ObjectMeta: metadata(), RoleRef: rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: registryAuthName}, Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: registryauth.ServiceAccountName, Namespace: env.Namespace}}}
	lease := &coordinationv1.Lease{TypeMeta: metav1.TypeMeta{APIVersion: "coordination.k8s.io/v1", Kind: "Lease"}, ObjectMeta: metav1.ObjectMeta{Name: registryauth.RestartLeaseName, Namespace: env.Namespace, Labels: labels}}
	workload := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metadata(), Spec: appsv1.DaemonSetSpec{
		Selector:       &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": registryAuthName}},
		UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1))}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels, Annotations: map[string]string{registryAuthHash: hash}}, Spec: corev1.PodSpec{
			NodeSelector:       map[string]string{"kubernetes.io/os": "linux", registryauth.FlexNodeLabel: registryauth.FlexNodeValue},
			Tolerations:        []corev1.Toleration{{Operator: corev1.TolerationOpExists}},
			ServiceAccountName: registryauth.ServiceAccountName, HostPID: true, HostNetwork: true, DNSPolicy: corev1.DNSClusterFirstWithHostNet,
			Containers: []corev1.Container{{
				Name: "configure", Image: env.Config.RegistryAuthNodeImage(), ImagePullPolicy: corev1.PullIfNotPresent,
				Args:            []string{"registry-auth-node"},
				Env:             []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}, {Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}}},
				SecurityContext: &corev1.SecurityContext{RunAsUser: ptr.To(int64(0)), RunAsNonRoot: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_CHROOT"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
				Ports:           []corev1.ContainerPort{{Name: "health", ContainerPort: 9096}},
				ReadinessProbe:  &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9096)}}, PeriodSeconds: 5},
				LivenessProbe:   &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/livez", Port: intstr.FromInt32(9096)}}, InitialDelaySeconds: 10, PeriodSeconds: 10},
				Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "host", MountPath: "/host"}, {Name: "config", MountPath: "/etc/registry-auth", ReadOnly: true}, {Name: "federated-token", MountPath: "/var/run/secrets/unbounded-registry-auth", ReadOnly: true}},
			}},
			Volumes: []corev1.Volume{
				{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/", Type: ptr.To(corev1.HostPathDirectory)}}},
				{Name: "config", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: registryAuthName}}}},
				{Name: "federated-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0o400)), Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: registryauth.FederatedAudience, ExpirationSeconds: ptr.To(int64(3600)), Path: "token"}}}}}},
			},
		}},
	}}

	return []client.Object{config, account, role, binding, lease, workload}, workload
}
