// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// DesiredDaemonSet declares the token audience, common keyring/trust projection,
// node-private identity and slab storage, socket mounts, and exclusion affinity.
// It must never introduce a per-node Secret or trust a node-name as a Node UID.
// This pure builder is consumed by the operator, never by controller startup.
func DesiredDaemonSet(c Config) (*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(c.DataplaneImage) == "" {
		return nil, fmt.Errorf("workload endpoint or image: %w", wire.InvalidRequest)
	}

	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return nil, fmt.Errorf("workload endpoint port: %w", wire.InvalidRequest)
		}
	}

	// The legacy managed-by label is part of the immutable selector. Preserve it
	// for in-place adoption; SSA's field manager records the actual workload owner.
	labels := map[string]string{"app.kubernetes.io/name": "racer-dataplane", "app.kubernetes.io/managed-by": "racer-controller"}
	// Keep diagnostics separate even when the configured peer port is 9090.
	diagnosticsPort := int32(9090)
	if c.PeerPort == uint16(diagnosticsPort) {
		diagnosticsPort++
	}

	ds := &appsv1.DaemonSet{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"}, ObjectMeta: metav1.ObjectMeta{Name: c.DaemonSetName, Namespace: c.Namespace, Labels: labels}, Spec: appsv1.DaemonSetSpec{
		Selector:       &metav1.LabelSelector{MatchLabels: labels},
		UpdateStrategy: appsv1.DaemonSetUpdateStrategy{Type: appsv1.RollingUpdateDaemonSetStrategyType, RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromInt32(0))}},
		// Require sustained readiness across probe periods before advancing a rollout.
		MinReadySeconds: 10,
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
			ServiceAccountName: c.DataplaneServiceAccount, AutomountServiceAccountToken: ptr.To(false),
			RestartPolicy: corev1.RestartPolicyAlways, DNSPolicy: corev1.DNSClusterFirst, SchedulerName: corev1.DefaultSchedulerName,
			EnableServiceLinks: ptr.To(false), PreemptionPolicy: ptr.To(corev1.PreemptLowerPriority),
			TerminationGracePeriodSeconds: ptr.To(int64(30)),
			SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
			Affinity:                      &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist}, {Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}}}}}}}},
			Containers: []corev1.Container{{
				Name: "dataplane", Image: c.DataplaneImage, ImagePullPolicy: corev1.PullIfNotPresent,
				TerminationMessagePath: corev1.TerminationMessagePathDefault, TerminationMessagePolicy: corev1.TerminationMessageReadFile,
				Env: []corev1.EnvVar{
					{Name: "RACER_CLUSTER_ID", Value: string(c.Cluster)},
					{Name: "RACER_CONTROL_ENDPOINT", Value: c.ControlURL},
					// This is only a bind address, never an authority for node identity.
					// Define it first so kubelet expands either Pod IP family below.
					{Name: "RACER_POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "status.podIP"}}},
					{Name: "RACER_PEER_LISTEN", Value: "[$(RACER_POD_IP)]:" + strconv.Itoa(int(c.PeerPort))},
					{Name: "RACER_DIAGNOSTICS_LISTEN", Value: "[$(RACER_POD_IP)]:" + strconv.Itoa(int(diagnosticsPort))},
					{Name: "RACER_TRUST_BUNDLE", Value: "/etc/racer/bootstrap/ca.crt"},
					{Name: "RACER_SERVICE_ACCOUNT_TOKEN", Value: "/var/run/racer-token/token"},
					{Name: "RACER_SECRET_DIRECTORY", Value: "/etc/racer/keyring"},
					// Kubelet creates the hostPath mount with mode 0755. Let the
					// dataplane create its private 0700 directory beneath it.
					{Name: "RACER_IDENTITY_DIRECTORY", Value: "/var/lib/racer/identity/private"},
					{Name: "RACER_SLAB_DIRECTORY", Value: "/var/lib/racer/slabs"},
				},
				Ports: []corev1.ContainerPort{{Name: "peer", ContainerPort: int32(c.PeerPort), Protocol: corev1.ProtocolTCP}, {Name: "diagnostics", ContainerPort: diagnosticsPort, Protocol: corev1.ProtocolTCP}},
				// Readiness may wait on enrollment/recovery indefinitely without probe
				// restarts. Membership must continue to include unready Pods.
				ReadinessProbe: &corev1.Probe{
					ProbeHandler:  corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromString("diagnostics"), Scheme: corev1.URISchemeHTTP}},
					PeriodSeconds: 5, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 1,
				},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
				VolumeMounts:    []corev1.VolumeMount{{Name: "token", MountPath: "/var/run/racer-token", ReadOnly: true}, {Name: "keyring", MountPath: "/etc/racer/keyring", ReadOnly: true}, {Name: "bootstrap", MountPath: "/etc/racer/bootstrap", ReadOnly: true}, {Name: "identity", MountPath: "/var/lib/racer/identity"}, {Name: "slabs", MountPath: "/var/lib/racer/slabs"}, {Name: "sockets", MountPath: "/run/racer"}},
			}},
			Volumes: []corev1.Volume{
				{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{DefaultMode: ptr.To(int32(0o400)), Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Audience: wire.TokenAudience, ExpirationSeconds: ptr.To(int64(3600)), Path: "token"}}}}}},
				{Name: "keyring", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: c.KeyringSecretName, DefaultMode: ptr.To(int32(0o400)), Items: []corev1.KeyToPath{{Key: "bundle.json", Path: "bundle.json"}}}}},
				{Name: "bootstrap", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: c.BootstrapTrustConfigMap}, DefaultMode: ptr.To(int32(0o444)), Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}}},
			},
		}},
	}}
	for _, mount := range []struct{ name, path string }{{"identity", "/var/lib/racer/identity"}, {"slabs", "/var/lib/racer/slabs"}, {"sockets", "/run/racer"}} {
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: mount.name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: mount.path, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	}

	return ds, nil
}
