// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/racer/wire"
)

// WorkloadConfig contains only the inputs needed to build the dataplane DaemonSet.
// Controller limits, rotation policy, serving TLS, and durable state are independent.
type WorkloadConfig struct {
	Cluster                 wire.ClusterID
	Namespace               string
	ControlURL              string
	BootstrapTrustConfigMap string
	DataplaneImage          string
	PeerPort                uint16
	HostNetwork             bool
	PodNetworkNodes         []string
	// Zero preserves the legacy automatic diagnostics port (9090, or 9091).
	DiagnosticsPort         uint16
	DataplaneServiceAccount string
	DaemonSetName           string
}

// WorkloadConfigFromLookup reads operator deployment wiring without loading or
// validating controller runtime configuration. Shared settings retain the same defaults.
func WorkloadConfigFromLookup(lookup func(string) (string, bool)) (WorkloadConfig, error) {
	env := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}

		return fallback
	}

	port, err := strconv.ParseUint(env("RACER_PEER_PORT", "8082"), 10, 16)
	if err != nil {
		return WorkloadConfig{}, fmt.Errorf("RACER_PEER_PORT: %w", wire.InvalidRequest)
	}

	hostNetwork := env("RACER_HOST_NETWORK", "false")
	if hostNetwork != "true" && hostNetwork != "false" {
		return WorkloadConfig{}, fmt.Errorf("RACER_HOST_NETWORK must be true or false: %w", wire.InvalidRequest)
	}

	var diagnosticsPort uint64
	if value, ok := lookup("RACER_DIAGNOSTICS_PORT"); ok {
		diagnosticsPort, err = strconv.ParseUint(value, 10, 16)
		if err != nil || diagnosticsPort < 1024 {
			return WorkloadConfig{}, fmt.Errorf("RACER_DIAGNOSTICS_PORT must be 1024..65535: %w", wire.InvalidRequest)
		}
	}

	var podNetworkNodes []string
	if value, ok := lookup("RACER_POD_NETWORK_NODES"); ok {
		if err := json.Unmarshal([]byte(value), &podNetworkNodes); err != nil || podNetworkNodes == nil {
			return WorkloadConfig{}, fmt.Errorf("RACER_POD_NETWORK_NODES must be a JSON array: %w", wire.InvalidRequest)
		}
	}

	cfg := WorkloadConfig{
		Cluster:                 wire.ClusterID(env("RACER_CLUSTER_ID", "")),
		Namespace:               env("POD_NAMESPACE", "unbounded-system"),
		ControlURL:              env("RACER_CONTROL_URL", ""),
		BootstrapTrustConfigMap: env("RACER_BOOTSTRAP_TRUST_CONFIGMAP", "racer-bootstrap-trust"),
		DataplaneImage:          env("RACER_DATAPLANE_IMAGE", ""),
		PeerPort:                uint16(port),
		HostNetwork:             hostNetwork == "true",
		PodNetworkNodes:         podNetworkNodes,
		DiagnosticsPort:         uint16(diagnosticsPort),
		DataplaneServiceAccount: env("RACER_DATAPLANE_SERVICE_ACCOUNT", "racer-dataplane"),
		DaemonSetName:           env("RACER_DAEMONSET_NAME", "racer-dataplane"),
	}

	return cfg, cfg.Validate()
}

func (c WorkloadConfig) Validate() error {
	if len(c.PodNetworkNodes) != 0 && (!c.HostNetwork || c.DaemonSetName != DataplaneDaemonSetName) {
		return fmt.Errorf("pod network exceptions require host networking and the fixed dataplane name: %w", wire.InvalidRequest)
	}

	seen := make(map[string]bool, len(c.PodNetworkNodes))
	for _, node := range c.PodNetworkNodes {
		if len(validation.IsDNS1123Subdomain(node)) != 0 || seen[node] {
			return fmt.Errorf("pod network nodes must be unique valid node names: %w", wire.InvalidRequest)
		}

		seen[node] = true
	}

	if !wire.ValidUUID(string(c.Cluster)) || len(validation.IsDNS1123Label(c.Namespace)) != 0 || c.PeerPort < 1024 {
		return fmt.Errorf("cluster, namespace, or peer port: %w", wire.InvalidRequest)
	}

	if c.DiagnosticsPort != 0 && (c.DiagnosticsPort < 1024 || c.DiagnosticsPort == c.PeerPort) {
		return fmt.Errorf("diagnostics port must be 1024..65535 and distinct from peer port: %w", wire.InvalidRequest)
	}

	for _, name := range []string{c.DaemonSetName, c.BootstrapTrustConfigMap, c.DataplaneServiceAccount} {
		if len(validation.IsDNS1123Subdomain(name)) != 0 {
			return fmt.Errorf("resource name: %w", wire.InvalidRequest)
		}
	}

	u, err := url.Parse(c.ControlURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || (u.Path != "" && u.Path != "/") || strings.TrimSpace(c.DataplaneImage) == "" {
		return fmt.Errorf("workload endpoint or image: %w", wire.InvalidRequest)
	}

	if u.Port() != "" {
		port, err := strconv.ParseUint(u.Port(), 10, 16)
		if err != nil || port == 0 {
			return fmt.Errorf("workload endpoint port: %w", wire.InvalidRequest)
		}
	}

	return nil
}

// DesiredDaemonSet declares the token audience, controller trust projection,
// node-private identity and slab storage, socket mounts, and exclusion affinity.
// It must never introduce a per-node Secret or trust a node-name as a Node UID.
// This pure builder is consumed by the operator, never by controller startup.
func DesiredDaemonSet(c WorkloadConfig) (*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	// Callers must implement drain-before-admit before opting into two workloads.
	if len(c.PodNetworkNodes) != 0 {
		return nil, fmt.Errorf("mixed networking requires the drain-aware two-workload planner: %w", wire.InvalidRequest)
	}

	// The legacy managed-by label is part of the immutable selector. Preserve it
	// for in-place adoption; SSA's field manager records the actual workload owner.
	labels := map[string]string{"app.kubernetes.io/name": "racer-dataplane", "app.kubernetes.io/managed-by": "racer-controller"}
	// Preserve the automatic port for existing installations; explicit ports
	// are validated rather than silently moved on collision.
	diagnosticsPort := int32(c.DiagnosticsPort)
	if diagnosticsPort == 0 {
		diagnosticsPort = 9090
		if c.PeerPort == uint16(diagnosticsPort) {
			diagnosticsPort++
		}
	}

	affinity := &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{
							Key:      wire.ExclusionLabel,
							Operator: corev1.NodeSelectorOpDoesNotExist,
						},
						{
							Key:      "kubernetes.io/os",
							Operator: corev1.NodeSelectorOpIn,
							Values:   []string{"linux"},
						},
					},
				}},
			},
		},
	}
	projectedVolumes := []corev1.Volume{
		{
			Name: "token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: ptr.To(int32(0o400)),
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          wire.TokenAudience,
							ExpirationSeconds: ptr.To(int64(3600)),
							Path:              "token",
						},
					}},
				},
			},
		},
		{
			Name: "bootstrap",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: c.BootstrapTrustConfigMap},
					DefaultMode:          ptr.To(int32(0o444)),
					Items: []corev1.KeyToPath{{
						Key:  "ca.crt",
						Path: "ca.crt",
					}},
				},
			},
		},
	}

	ds := &appsv1.DaemonSet{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "apps/v1",
			Kind:       "DaemonSet",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.DaemonSetName,
			Namespace: c.Namespace,
			Labels:    labels,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
				},
			},
			// Require sustained readiness across probe periods before advancing a rollout.
			MinReadySeconds: 10,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName:            c.DataplaneServiceAccount,
					AutomountServiceAccountToken:  ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyAlways,
					DNSPolicy:                     corev1.DNSClusterFirst,
					SchedulerName:                 corev1.DefaultSchedulerName,
					EnableServiceLinks:            ptr.To(false),
					PreemptionPolicy:              ptr.To(corev1.PreemptLowerPriority),
					TerminationGracePeriodSeconds: ptr.To(int64(30)),
					SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
					Affinity:                      affinity,
					Containers: []corev1.Container{{
						Name:                     "dataplane",
						Image:                    c.DataplaneImage,
						ImagePullPolicy:          corev1.PullIfNotPresent,
						TerminationMessagePath:   corev1.TerminationMessagePathDefault,
						TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						Env: []corev1.EnvVar{
							{
								Name:  "RACER_CLUSTER_ID",
								Value: string(c.Cluster),
							},
							{
								Name:  "RACER_CONTROL_ENDPOINT",
								Value: c.ControlURL,
							},
							// This is only a bind address, never an authority for node identity.
							// Define it first so kubelet expands either Pod IP family below.
							{
								Name: "RACER_POD_IP",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										APIVersion: "v1",
										FieldPath:  "status.podIP",
									},
								},
							},
							{
								Name:  "RACER_PEER_LISTEN",
								Value: "[$(RACER_POD_IP)]:" + strconv.Itoa(int(c.PeerPort)),
							},
							{
								Name:  "RACER_DIAGNOSTICS_LISTEN",
								Value: "[$(RACER_POD_IP)]:" + strconv.Itoa(int(diagnosticsPort)),
							},
							{
								Name:  "RACER_TRUST_BUNDLE",
								Value: "/etc/racer/bootstrap/ca.crt",
							},
							{
								Name:  "RACER_SERVICE_ACCOUNT_TOKEN",
								Value: "/var/run/racer-token/token",
							},
							// Kubelet creates the hostPath mount with mode 0755. Let the
							// dataplane create its private 0700 directory beneath it.
							{
								Name:  "RACER_IDENTITY_DIRECTORY",
								Value: "/var/lib/racer/identity/private",
							},
							{
								Name:  "RACER_SLAB_DIRECTORY",
								Value: "/var/lib/racer/slabs",
							},
						},
						Ports: []corev1.ContainerPort{
							{
								Name:          "peer",
								ContainerPort: int32(c.PeerPort),
								Protocol:      corev1.ProtocolTCP,
							},
							{
								Name:          "diagnostics",
								ContainerPort: diagnosticsPort,
								Protocol:      corev1.ProtocolTCP,
							},
						},
						// Readiness may wait on enrollment/recovery indefinitely without probe
						// restarts. Membership must continue to include unready Pods.
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path:   "/readyz",
									Port:   intstr.FromString("diagnostics"),
									Scheme: corev1.URISchemeHTTP,
								},
							},
							PeriodSeconds:    5,
							TimeoutSeconds:   2,
							SuccessThreshold: 1,
							FailureThreshold: 1,
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "token",
								MountPath: "/var/run/racer-token",
								ReadOnly:  true,
							},
							{
								Name:      "bootstrap",
								MountPath: "/etc/racer/bootstrap",
								ReadOnly:  true,
							},
							{
								Name:      "identity",
								MountPath: "/var/lib/racer/identity",
							},
							{
								Name:      "slabs",
								MountPath: "/var/lib/racer/slabs",
							},
							{
								Name:      "sockets",
								MountPath: "/run/racer",
							},
						},
					}},
					Volumes: projectedVolumes,
				},
			},
		},
	}

	if c.HostNetwork {
		ds.Spec.Template.Spec.HostNetwork = true
		ds.Spec.Template.Spec.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}

	for _, mount := range []struct{ name, path string }{{"identity", "/var/lib/racer/identity"}, {"slabs", "/var/lib/racer/slabs"}, {"sockets", "/run/racer"}} {
		ds.Spec.Template.Spec.Volumes = append(ds.Spec.Template.Spec.Volumes, corev1.Volume{Name: mount.name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: mount.path, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	}

	return ds, nil
}

// DesiredDaemonSets builds steady-state placement, not a safe migration plan.
// The operator must additionally exclude occupied destination nodes until every
// source Pod, including terminating Pods, has disappeared.
func DesiredDaemonSets(c WorkloadConfig) ([]*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	nodes := slices.Clone(c.PodNetworkNodes)
	slices.Sort(nodes)

	c.PodNetworkNodes = nil

	host, err := DesiredDaemonSet(c)
	if err != nil {
		return nil, err
	}

	if len(nodes) == 0 {
		return []*appsv1.DaemonSet{host}, nil
	}

	c.HostNetwork = false
	c.DaemonSetName = PodNetworkDaemonSetName

	pod, err := DesiredDaemonSet(c)
	if err != nil {
		return nil, err
	}

	// The existing selector is immutable. Use a distinct app value, not an
	// additional label that would still match the original workload selector.
	pod.Labels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	pod.Spec.Selector.MatchLabels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	pod.Spec.Template.Labels["app.kubernetes.io/name"] = PodNetworkDaemonSetName
	hostSelector := host.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	podSelector := pod.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
	base := podSelector.NodeSelectorTerms[0].DeepCopy()
	podSelector.NodeSelectorTerms = nil
	// Field selectors accept one value per requirement. Host exclusions are
	// ANDed; each pod-network node gets an OR term retaining the base constraints.
	for _, node := range nodes {
		hostSelector.NodeSelectorTerms[0].MatchFields = append(hostSelector.NodeSelectorTerms[0].MatchFields, corev1.NodeSelectorRequirement{
			Key: "metadata.name", Operator: corev1.NodeSelectorOpNotIn, Values: []string{node},
		})
		term := base.DeepCopy()
		term.MatchFields = []corev1.NodeSelectorRequirement{{Key: "metadata.name", Operator: corev1.NodeSelectorOpIn, Values: []string{node}}}
		podSelector.NodeSelectorTerms = append(podSelector.NodeSelectorTerms, *term)
	}

	return []*appsv1.DaemonSet{host, pod}, nil
}
