// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"crypto/sha256"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	unboundedv1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1alpha1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/operator/component"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

const (
	controlPlaneName            = "racer-controlplane"
	dataplaneName               = "racer-dataplane"
	dataplaneDaemonSetPrefix    = "racer-"
	stateRoleName               = "racer-controlplane-state"
	bootstrapRoleName           = "racer-bootstrap"
	bootstrapControllerRoleName = "racer-bootstrap-controller"
	componentLabel              = racermeta.MetadataPrefix + "component"
)

// SiteDaemonSetName preserves short DNS-label Site names. Names requiring encoding
// use a dot-separated digest suffix, which cannot collide with the plain form.
func SiteDaemonSetName(site string) string {
	name := dataplaneDaemonSetPrefix + site
	if len(name) <= 63 && len(validation.IsDNS1123Label(site)) == 0 {
		return name
	}

	sum := sha256.Sum256([]byte(site))

	return fmt.Sprintf("%ssite.%x", dataplaneDaemonSetPrefix, sum[:16])
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
// controller owns runtime ConfigMaps, leader Leases and the CA Secret; emitting
// any of those here would replace durable state or rotate identities on upgrade.
func sharedResources(namespace string) []client.Object {
	return []client.Object{
		serviceAccount(controlPlaneName, namespace),
		clusterRole(controlPlaneName, controlPlaneName,
			rbacv1.PolicyRule{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes", "pods"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{unboundedv1alpha3.GroupVersion.Group}, Resources: []string{"sites"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{racerv1alpha1.GroupName}, Resources: []string{"p2pcaches"}, Verbs: []string{"get", "list", "watch"}},
			rbacv1.PolicyRule{APIGroups: []string{racerv1alpha1.GroupName}, Resources: []string{"p2pcaches/status"}, Verbs: []string{"get", "patch", "update"}},
			// Storage status is published as Node metadata annotations, not nodes/status.
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"patch"}},
		),
		clusterBinding(controlPlaneName, namespace, controlPlaneName),
		role(stateRoleName, namespace, controlPlaneName,
			// Same-Pod dataplane restarts require graceful Pod replacement before
			// their ambiguous boot identities can leave the CA rotation barrier.
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"delete"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "list", "watch", "create", "update", "delete"}},
			rbacv1.PolicyRule{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"get", "list", "watch", "create", "update", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"events"}, Verbs: []string{"create", "patch"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, ResourceNames: []string{"racer-ca"}, Verbs: []string{"get", "update"}},
			rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"secrets"}, Verbs: []string{"create"}},
			rbacv1.PolicyRule{APIGroups: []string{"apps"}, Resources: []string{"daemonsets", "replicasets", "deployments"}, Verbs: []string{"get"}},
		),
		roleBinding(stateRoleName, namespace, controlPlaneName),
		serviceAccount(dataplaneName, namespace),
		clusterRole(bootstrapRoleName, dataplaneName, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"get"}}),
		clusterBinding(bootstrapRoleName, namespace, dataplaneName),
		role(bootstrapControllerRoleName, namespace, dataplaneName, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"services"}, ResourceNames: []string{controlPlaneName}, Verbs: []string{"get"}}),
		roleBinding(bootstrapControllerRoleName, namespace, dataplaneName),
		controlService(namespace),
		controlDisruptionBudget(namespace),
	}
}

func controlService(namespace string) *corev1.Service {
	return &corev1.Service{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}, ObjectMeta: metadata(controlPlaneName, namespace, controlPlaneName),
		Spec: corev1.ServiceSpec{Selector: map[string]string{componentLabel: controlPlaneName, racermeta.MetadataPrefix + "serving-leader": "true"}, Ports: []corev1.ServicePort{
			{Name: "subscription", Port: 8443, TargetPort: intstr.FromString("subscription"), Protocol: corev1.ProtocolTCP},
			{Name: "enrollment", Port: 8444, TargetPort: intstr.FromString("enrollment"), Protocol: corev1.ProtocolTCP},
			{Name: "trust-proof", Port: 8446, TargetPort: intstr.FromString("trust-proof"), Protocol: corev1.ProtocolTCP},
		}},
	}
}

func controlDisruptionBudget(namespace string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		TypeMeta: metav1.TypeMeta{APIVersion: "policy/v1", Kind: "PodDisruptionBudget"}, ObjectMeta: metadata(controlPlaneName, namespace, controlPlaneName),
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: ptr.To(intstr.FromInt32(1)), Selector: &metav1.LabelSelector{MatchLabels: map[string]string{componentLabel: controlPlaneName}}},
	}
}

func controlDeployment(namespace string, cfg component.Config) *appsv1.Deployment {
	labels := map[string]string{componentLabel: controlPlaneName}

	return &appsv1.Deployment{
		TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}, ObjectMeta: metadata(controlPlaneName, namespace, controlPlaneName),
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(2)), Selector: &metav1.LabelSelector{MatchLabels: labels},
			// Ready includes warm standbys; Service membership is leader-only.
			// Keep one available replica, including upgrades from leader-only
			// readiness where the old ReplicaSet has only one available Pod.
			MinReadySeconds: 10,
			Strategy:        appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType, RollingUpdate: &appsv1.RollingUpdateDeployment{MaxSurge: ptr.To(intstr.FromInt32(1)), MaxUnavailable: ptr.To(intstr.FromInt32(1))}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				ServiceAccountName: controlPlaneName,
				// Soft constraints allow a surge on two-node clusters and degraded
				// operation when a failure domain is unavailable.
				TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
					{MaxSkew: 1, TopologyKey: corev1.LabelHostname, WhenUnsatisfiable: corev1.ScheduleAnyway, LabelSelector: &metav1.LabelSelector{MatchLabels: labels}},
					{MaxSkew: 1, TopologyKey: corev1.LabelTopologyZone, WhenUnsatisfiable: corev1.ScheduleAnyway, LabelSelector: &metav1.LabelSelector{MatchLabels: labels}},
				},
				Containers: []corev1.Container{{
					Name: "controller", Image: cfg.Image(controlPlaneName),
					Args:           []string{"-state-namespace=" + namespace},
					Env:            append(podIdentityEnv(), corev1.EnvVar{Name: "RACER_TLS_TRUST_DIR", Value: "/var/run/racer-trust"}),
					Ports:          []corev1.ContainerPort{{Name: "subscription", ContainerPort: 8443, Protocol: corev1.ProtocolTCP}, {Name: "enrollment", ContainerPort: 8444, Protocol: corev1.ProtocolTCP}, {Name: "replica-proof", ContainerPort: 8445, Protocol: corev1.ProtocolTCP}, {Name: "trust-proof", ContainerPort: 8446, Protocol: corev1.ProtocolTCP}, {Name: "health", ContainerPort: 8081, Protocol: corev1.ProtocolTCP}},
					ReadinessProbe: httpProbe("/readyz", intstr.FromString("health")),
					LivenessProbe:  httpProbe("/healthz", intstr.FromString("health")),
					Resources:      corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
					VolumeMounts:   []corev1.VolumeMount{trustMount()},
				}},
				// The controller creates the trust bundle on the first startup.
				Volumes: []corev1.Volume{trustVolume(true)},
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

func podIdentityEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		fieldEnv("RACER_POD_NAME", "metadata.name"),
		fieldEnv("RACER_POD_NAMESPACE", "metadata.namespace"),
		fieldEnv("RACER_POD_UID", "metadata.uid"),
	}
}

func securityContext(bootstrap bool) *corev1.SecurityContext {
	s := &corev1.SecurityContext{
		RunAsUser: ptr.To(int64(0)), RunAsGroup: ptr.To(int64(65532)), Privileged: ptr.To(false), AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
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
	// Static across capacity edits. The Rust populated_two_tib_memory fixture
	// measures <1 GiB index RSS with full payload descriptors, metadata admission
	// and three tree versions. Scaling to 4 TiB allows 2 GiB, plus <512 MiB for
	// empty replacement/checkpoint drain and 1.5 GiB for pools, process/network
	// state and allocator variation. This is an operational envelope, not the
	// diagnostic sparse-tree worst case. Resize also checks current headroom.
	// Equal requests/limits (including init) preserve Guaranteed QoS.
	r := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("4Gi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("4Gi")},
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
			// The hostPath is root-owned. Its owner can assign its own effective
			// group without CAP_CHOWN; setgid propagates that group to cache dirs.
			"chgrp 65532 /dev/racer", "chmod 2770 /dev/racer",
			`export RACER_CONTROL_PLANE_URL="https://racer-controlplane.` + namespace + `.svc:8443/v3/$RACER_UNIVERSE/$RACER_NODE"`,
			"exec /usr/local/bin/racer-dataplane",
		}, "\n")},
		SecurityContext: securityContext(false), Resources: dataplaneResources(false),
		Env: append(podIdentityEnv(), []corev1.EnvVar{
			{Name: "RACER_CONTROL_TOKEN_FILE", Value: "/var/run/racer-control/token"},
			{Name: "RACER_TLS_TRUST_DIR", Value: "/var/run/racer-trust"},
			{Name: "RACER_ENROLL_URL", Value: "https://racer-controlplane." + namespace + ".svc:8444/v3/enroll"},
			{Name: "RACER_CONTROL_SERVER_NAME", Value: "racer-controlplane." + namespace + ".svc"},
			fieldEnv("RACER_POD_IP", "status.podIP"),
			{Name: "RACER_SLAB_PATH", Value: "/cache/cache.slab"},
			// Creation defaults only. TLS-authenticated storage policy owns subsequent capacity;
			// persisted geometry wins on restart. Keep the execution cap at one even
			// when automatic runtime storage planning creates hundreds of shards.
			{Name: "RACER_SLAB_SIZE", Value: "10737418240"},
			{Name: "RACER_SHARDS", Value: "1"},
			{Name: "RACER_IO_WORKERS", Value: "1"},
			{Name: "RACER_COMPUTE_WORKERS", Value: "1"},
			{Name: "RACER_BUFFERS_PER_NODE", Value: "8"},
			{Name: "RACER_STARTUP_SECONDS", Value: "90"},
			{Name: "RACER_STALL_SECONDS", Value: "5"},
			{Name: "RACER_DRAIN_SECONDS", Value: "20"},
			{Name: "RACER_QUIESCE_SECONDS", Value: "5"},
		}...),
		StartupProbe: httpProbe("/startupz", intstr.FromInt32(9090)), ReadinessProbe: httpProbe("/readyz", intstr.FromInt32(9090)), LivenessProbe: httpProbe("/livez", intstr.FromInt32(9090)),
		VolumeMounts: []corev1.VolumeMount{
			{Name: "control-token", MountPath: "/var/run/racer-control", ReadOnly: true},
			trustMount(),
			{Name: "bootstrap", MountPath: "/bootstrap", ReadOnly: true},
			{Name: "cache", MountPath: "/cache"},
			{Name: "sockets", MountPath: racermeta.SocketRoot},
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
					trustVolume(false),
					{Name: "bootstrap", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					{Name: "cache", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/var/lib/racer", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}},
					{Name: "sockets", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: racermeta.SocketRoot, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}},
				},
			}},
		},
	}
}

func trustMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: "trust", MountPath: "/var/run/racer-trust", ReadOnly: true}
}

func trustVolume(optional bool) corev1.Volume {
	return corev1.Volume{Name: "trust", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "racer-trust"}, Items: []corev1.KeyToPath{{Key: "bundle.json", Path: "bundle.json"}}, Optional: ptr.To(optional)}}}
}
