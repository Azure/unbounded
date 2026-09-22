// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/sha256"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const (
	controlPlaneName            = "racer-controlplane"
	dataplaneName               = "racer-dataplane"
	stateRoleName               = "racer-controlplane-state"
	bootstrapRoleName           = "racer-bootstrap"
	bootstrapControllerRoleName = "racer-bootstrap-controller"
	componentLabel              = racermeta.MetadataPrefix + "component"
)

// SiteDaemonSetName preserves short DNS-label Site names. Names requiring encoding
// use a dot-separated digest suffix, which cannot collide with the plain form.
func SiteDaemonSetName(site string) string {
	name := dataplaneName + "-" + site
	if len(name) <= 63 && len(validation.IsDNS1123Label(site)) == 0 {
		return name
	}

	sum := sha256.Sum256([]byte(site))

	return fmt.Sprintf("%s-site.%x", dataplaneName, sum[:16])
}

func metadata(name, namespace, part string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{componentLabel: part}}
}

func serviceAccount(name, namespace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metadata(name, namespace, name)}
}

func clusterRole(name, part string, rules ...rbacv1.PolicyRule) *rbacv1.ClusterRole {
	return &rbacv1.ClusterRole{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRole"}, ObjectMeta: metadata(name, "", part), Rules: rules}
}

func role(name, namespace, part string, rules ...rbacv1.PolicyRule) *rbacv1.Role {
	return &rbacv1.Role{TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "Role"}, ObjectMeta: metadata(name, namespace, part), Rules: rules}
}

func clusterBinding(name, namespace, account string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "ClusterRoleBinding"}, ObjectMeta: metadata(name, "", account),
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: namespace}},
	}
}

func roleBinding(name, namespace, account string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: rbacv1.SchemeGroupVersion.String(), Kind: "RoleBinding"}, ObjectMeta: metadata(name, namespace, account),
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name},
		Subjects: []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: namespace}},
	}
}

// sharedResources contains only operator-owned installation resources. The Racer
// controller owns runtime ConfigMaps, leader Leases and signing Secrets; emitting
// any of those here would replace durable state or rotate identities on upgrade.
func sharedResources(namespace string) []client.Object {
	return []client.Object{
		serviceAccount(controlPlaneName, namespace),
		clusterRole(controlPlaneName, controlPlaneName,
			rbacv1.PolicyRule{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes", "pods", "services"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"}, Verbs: []string{"patch"}},
		),
		clusterBinding(controlPlaneName, namespace, controlPlaneName),
		role(stateRoleName, namespace, controlPlaneName,
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			rbacv1.PolicyRule{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"racer-config-signing", "racer-peer-signing"}, Verbs: []string{"get", "update"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create", "list", "watch"}},
		),
		roleBinding(stateRoleName, namespace, controlPlaneName),
		serviceAccount(dataplaneName, namespace),
		clusterRole(bootstrapRoleName, dataplaneName, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}}),
		clusterBinding(bootstrapRoleName, namespace, dataplaneName),
		role(bootstrapControllerRoleName, namespace, dataplaneName, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"}, ResourceNames: []string{controlPlaneName}, Verbs: []string{"get"}}),
		roleBinding(bootstrapControllerRoleName, namespace, dataplaneName),
		controlService(namespace),
	}
}

func controlService(namespace string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metadata(controlPlaneName, namespace, controlPlaneName),
		Spec: corev1.ServiceSpec{Selector: map[string]string{componentLabel: controlPlaneName}, Ports: []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromString("subscription"), Protocol: corev1.ProtocolTCP}}},
	}
}

func controlDeployment(namespace string, cfg component.Config) *appsv1.Deployment {
	labels := map[string]string{componentLabel: controlPlaneName}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metadata(controlPlaneName, namespace, controlPlaneName),
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			// Only the leader serves /readyz. Requiring every replica to be ready
			// would deadlock rolling replacement of intentional standby replicas.
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr.To(intstr.FromInt32(0)), MaxUnavailable: ptr.To(intstr.FromInt32(2))}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				ServiceAccountName: controlPlaneName,
				Containers: []corev1.Container{{
					Name: "controller", Image: cfg.Image(controlPlaneName),
					Args:           []string{"-state-namespace=" + namespace, "-reserved-management-ports=9090"},
					Ports:          []corev1.ContainerPort{{Name: "subscription", ContainerPort: 8080, Protocol: corev1.ProtocolTCP}, {Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP}},
					ReadinessProbe: httpProbe("/readyz", intstr.FromString("subscription")),
					LivenessProbe:  httpProbe("/healthz", intstr.FromString("health")),
					Resources:      corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
				}},
			}},
		},
	}
}

func httpProbe(path string, port intstr.IntOrString) *corev1.Probe {
	return &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: path, Port: port, Scheme: corev1.URISchemeHTTP}}}
}

func fieldEnv(name, field string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: field}}}
}

func securityContext(bootstrap bool) *corev1.SecurityContext {
	s := &corev1.SecurityContext{
		RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(0)), Privileged: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
		Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}, Add: []corev1.Capability{"SYS_RESOURCE"}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeUnconfined},
	}
	if bootstrap {
		s.RunAsUser, s.RunAsGroup, s.RunAsNonRoot = ptr.To(int64(65532)), ptr.To(int64(65532)), ptr.To(true)
		s.Capabilities.Add = nil
		s.SeccompProfile.Type = corev1.SeccompProfileTypeRuntimeDefault
	}

	return s
}

func dataplaneResources(bootstrap bool) corev1.ResourceRequirements {
	r := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("2Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("2Gi")},
	}
	if !bootstrap {
		r.Requests[corev1.ResourceEphemeralStorage] = resource.MustParse("128Mi")
		r.Limits[corev1.ResourceEphemeralStorage] = resource.MustParse("512Mi")
	}

	return r
}

// dataplaneDaemonSet derives both scheduling and bootstrap identity from the Site.
// The old node annotation/label mirror admission guard is obsolete: neither is
// an identity authority now. RequiredNodeAffinity and bootstrap's Site check use
// canonical-first membership, and exclusions apply to both fallback branches.
func dataplaneDaemonSet(namespace string, cfg component.Config, site *unboundedv1alpha3.Site) *appsv1.DaemonSet {
	labels := map[string]string{racermeta.DataplaneLabelKey: "true", racermeta.UniverseKey: racermeta.UniverseForSite(site.Name)}
	meta := metadata(SiteDaemonSetName(site.Name), namespace, dataplaneName)
	meta.OwnerReferences = []metav1.OwnerReference{component.SiteOwnerReference(site)}
	main := corev1.Container{
		Name: "dataplane", Image: cfg.Image(dataplaneName), Command: []string{"/bin/sh", "-ec"},
		Args: []string{strings.Join([]string{
			"ulimit -l 262144", ". /bootstrap/identity",
			`export RACER_CONTROL_PLANE_URL="http://$RACER_CONTROL_ADDRESS/v2/$RACER_UNIVERSE/$RACER_NODE"`,
			"exec /usr/local/bin/racer-dataplane",
		}, "\n")},
		SecurityContext: securityContext(false), Resources: dataplaneResources(false),
		Env: []corev1.EnvVar{
			{Name: "RACER_CONTROL_TOKEN_FILE", Value: "/var/run/racer-control/token"},
			{Name: "RACER_PEER_KEYS_DIR", Value: "/var/run/racer-peer-signing"},
			{Name: "RACER_CONFIG_KEYS_DIR", Value: "/var/run/racer-config-verify"},
			fieldEnv("RACER_POD_IP", "status.podIP"),
			{Name: "RACER_SLAB_PATH", Value: "/cache/cache.slab"},
			{Name: "RACER_SLAB_SIZE", Value: "10737418240"},
			{Name: "RACER_SHARDS", Value: "1"},
			{Name: "RACER_IO_WORKERS", Value: "1"},
			{Name: "RACER_COMPUTE_WORKERS", Value: "1"},
			{Name: "RACER_BUFFERS_PER_NODE", Value: "8"},
			{Name: "RACER_STARTUP_SECONDS", Value: "90"},
			{Name: "RACER_STALL_SECONDS", Value: "5"},
			{Name: "RACER_DRAIN_SECONDS", Value: "20"},
			{Name: "RACER_QUIESCE_SECONDS", Value: "5"},
		},
		StartupProbe: httpProbe("/startupz", intstr.FromInt32(9090)), ReadinessProbe: httpProbe("/readyz", intstr.FromInt32(9090)), LivenessProbe: httpProbe("/livez", intstr.FromInt32(9090)),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "control-token", MountPath: "/var/run/racer-control", ReadOnly: true},
			{Name: "peer-signing", MountPath: "/var/run/racer-peer-signing", ReadOnly: true},
			{Name: "config-verify", MountPath: "/var/run/racer-config-verify", ReadOnly: true},
			{Name: "bootstrap", MountPath: "/bootstrap", ReadOnly: true},
			{Name: "cache", MountPath: "/cache"},
		},
	}
	main.StartupProbe.PeriodSeconds, main.StartupProbe.FailureThreshold = 2, 90
	main.ReadinessProbe.PeriodSeconds = 2
	bootstrap := corev1.Container{
		Name: "bootstrap", Image: cfg.Image(controlPlaneName), Command: []string{"/bin/sh", "-ec"},
		Args:            []string{`/usr/local/bin/racer-controlplane -bootstrap-node="$NODE_NAME" -bootstrap-universe="$POD_UNIVERSE" -bootstrap-namespace="$POD_NAMESPACE" -bootstrap-service=racer-controlplane > /bootstrap/identity`},
		Env:             []corev1.EnvVar{fieldEnv("POD_IP", "status.podIP"), fieldEnv("NODE_NAME", "spec.nodeName"), fieldEnv("POD_NAMESPACE", "metadata.namespace"), {Name: "POD_UNIVERSE", Value: racermeta.UniverseForSite(site.Name)}},
		SecurityContext: securityContext(true), Resources: dataplaneResources(true), VolumeMounts: []corev1.VolumeMount{{Name: "bootstrap", MountPath: "/bootstrap"}},
	}

	return &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: meta,
		Spec: appsv1.DaemonSetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxSurge: ptr.To(intstr.FromInt32(0)), MaxUnavailable: ptr.To(intstr.FromInt32(1))}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				ServiceAccountName: dataplaneName, TerminationGracePeriodSeconds: ptr.To(int64(35)), NodeSelector: map[string]string{corev1.LabelOSStable: "linux"},
				Affinity: &corev1.Affinity{NodeAffinity: racermeta.RequiredNodeAffinity(site.Name)}, SecurityContext: &corev1.PodSecurityContext{FSGroup: ptr.To(int64(65532))},
				InitContainers: []corev1.Container{bootstrap}, Containers: []corev1.Container{main},
				Volumes: []corev1.Volume{
					{Name: "control-token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "racer-control", ExpirationSeconds: ptr.To(int64(3600))}}}}}},
					bundleVolume("peer-signing", "racer-peer-signing"), bundleVolume("config-verify", "racer-config-signing"),
					{Name: "bootstrap", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "cache", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/racer", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}},
				},
			}},
		},
	}
}

func bundleVolume(name, secret string) corev1.Volume {
	return corev1.Volume{Name: name, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: secret, Items: []corev1.KeyToPath{{Key: "bundle.json", Path: "bundle.json"}}}}}
}
