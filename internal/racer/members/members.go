// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package members builds dataplane workloads and derives deterministic membership
// candidates from observed Kubernetes values. Discovery uses caller-provided
// readers; this package owns no clients, publication state, or annotation writes.
package members

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machinav1 "github.com/Azure/unbounded/api/machina/v1alpha3"
	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/wire"
)

// Workload configuration and construction.

const (
	DataplaneDaemonSetName  = "racer-dataplane"
	PodNetworkDaemonSetName = "racer-dataplane-podnet"
)

// ManagedNames contains only the explicitly configured workload.
func ManagedNames(daemonSetName string) []string {
	return []string{daemonSetName}
}

// ReadWorkloadIdentities snapshots current DaemonSet ownership for one discovery
// or authorization pass. Missing and terminating workloads grant no ownership.
func ReadWorkloadIdentities(ctx context.Context, reader client.Reader, namespace, daemonSetName string) (WorkloadIdentities, error) {
	ids := WorkloadIdentities{Namespace: namespace, Name: daemonSetName}

	var ds appsv1.DaemonSet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: daemonSetName}, &ds); err != nil {
		if apierrors.IsNotFound(err) {
			return ids, nil
		}

		return WorkloadIdentities{}, err
	}

	if ds.DeletionTimestamp == nil {
		ids.UID = ds.UID
	}

	return ids, nil
}

// ControllerPod checks the live namespace and service-account identity shared by
// replication discovery and authorization. Readiness is not an identity signal.
func ControllerPod(pod *corev1.Pod, namespace, serviceAccount string) bool {
	return pod.Namespace == namespace && pod.UID != "" && pod.DeletionTimestamp == nil && pod.Spec.ServiceAccountName == serviceAccount && pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

// Config contains only the inputs needed to build the dataplane DaemonSet.
// Controller limits, rotation policy, serving TLS, and durable state are independent.
type Config struct {
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

// ConfigFromLookup reads operator deployment wiring without loading or
// validating controller runtime configuration. Shared settings retain the same defaults.
func ConfigFromLookup(lookup func(string) (string, bool)) (Config, error) {
	env := func(key, fallback string) string {
		if value, ok := lookup(key); ok {
			return value
		}

		return fallback
	}

	port, err := strconv.ParseUint(env("RACER_PEER_PORT", "8082"), 10, 16)
	if err != nil {
		return Config{}, fmt.Errorf("RACER_PEER_PORT: %w", wire.InvalidRequest)
	}

	hostNetwork := env("RACER_HOST_NETWORK", "false")
	if hostNetwork != "true" && hostNetwork != "false" {
		return Config{}, fmt.Errorf("RACER_HOST_NETWORK must be true or false: %w", wire.InvalidRequest)
	}

	var diagnosticsPort uint64
	if value, ok := lookup("RACER_DIAGNOSTICS_PORT"); ok {
		diagnosticsPort, err = strconv.ParseUint(value, 10, 16)
		if err != nil || diagnosticsPort < 1024 {
			return Config{}, fmt.Errorf("RACER_DIAGNOSTICS_PORT must be 1024..65535: %w", wire.InvalidRequest)
		}
	}

	var podNetworkNodes []string
	if value, ok := lookup("RACER_POD_NETWORK_NODES"); ok {
		if err := json.Unmarshal([]byte(value), &podNetworkNodes); err != nil || podNetworkNodes == nil {
			return Config{}, fmt.Errorf("RACER_POD_NETWORK_NODES must be a JSON array: %w", wire.InvalidRequest)
		}
	}

	cfg := Config{
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

func (c Config) Validate() error {
	if err := c.validateNetworkNodes(); err != nil {
		return err
	}

	if err := c.validateIdentityAndPorts(); err != nil {
		return err
	}

	return c.validateEndpoint()
}

func (c Config) validateNetworkNodes() error {
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

	return nil
}

func (c Config) validateIdentityAndPorts() error {
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

	// The workload name is also the immutable instance selector label value.
	if len(validation.IsValidLabelValue(c.DaemonSetName)) != 0 {
		return fmt.Errorf("DaemonSet name must fit a label value: %w", wire.InvalidRequest)
	}

	return nil
}

func (c Config) validateEndpoint() error {
	u, err := url.Parse(c.ControlURL)
	if err != nil {
		return fmt.Errorf("workload endpoint or image: %w", wire.InvalidRequest)
	}

	validOrigin := u.Scheme == "https" && u.Hostname() != "" && u.User == nil

	plainRoot := u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.RawPath == "" && (u.Path == "" || u.Path == "/")
	if !validOrigin || !plainRoot || strings.TrimSpace(c.DataplaneImage) == "" {
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
func DesiredDaemonSet(c Config) (*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	// Callers must implement drain-before-admit before opting into two workloads.
	if len(c.PodNetworkNodes) != 0 {
		return nil, fmt.Errorf("mixed networking requires the drain-aware two-workload planner: %w", wire.InvalidRequest)
	}

	return buildDaemonSet(c), nil
}

func buildDaemonSet(c Config) *appsv1.DaemonSet {
	// Use workload identity, not the manager's identity, for the Pod selector.
	labels := map[string]string{"app.kubernetes.io/name": "racer-dataplane", "app.kubernetes.io/instance": c.DaemonSetName}

	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: c.DaemonSetName, Namespace: c.Namespace, Labels: labels},
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
			Template:        corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: dataplanePod(c)},
		},
	}
}

func dataplanePod(c Config) corev1.PodSpec {
	pod := corev1.PodSpec{
		ServiceAccountName:            c.DataplaneServiceAccount,
		AutomountServiceAccountToken:  ptr.To(false),
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		EnableServiceLinks:            ptr.To(false),
		PreemptionPolicy:              ptr.To(corev1.PreemptLowerPriority),
		TerminationGracePeriodSeconds: ptr.To(int64(30)),
		SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
		Affinity:                      dataplaneAffinity(),
		Containers:                    []corev1.Container{dataplaneContainer(c)},
		Volumes:                       dataplaneVolumes(c),
	}
	if c.HostNetwork {
		pod.HostNetwork = true
		pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}

	return pod
}

func diagnosticsPort(c Config) int32 {
	// Preserve the automatic port for existing installations; explicit ports
	// are validated rather than silently moved on collision.
	diagnosticsPort := int32(c.DiagnosticsPort)
	if diagnosticsPort == 0 {
		diagnosticsPort = 9090
		if c.PeerPort == uint16(diagnosticsPort) {
			diagnosticsPort++
		}
	}

	return diagnosticsPort
}

func dataplaneAffinity() *corev1.Affinity {
	return &corev1.Affinity{
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
}

func dataplaneVolumes(c Config) []corev1.Volume {
	volumes := []corev1.Volume{
		{
			Name: "devices",
			VolumeSource: corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/dev", Type: ptr.To(corev1.HostPathDirectory)},
			},
		},
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

	// DirectoryOrCreate also permits HTTP-only nodes without RDMA hardware.
	// Kubelet creates an empty /dev/infiniband; it does not create device nodes.
	for _, mount := range []struct{ name, path string }{{"identity", "/var/lib/racer/identity"}, {"slabs", "/var/lib/racer/slabs"}, {"sockets", "/run/racer"}, {"infiniband", "/dev/infiniband"}} {
		volumes = append(volumes, corev1.Volume{Name: mount.name, VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: mount.path, Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
	}

	return volumes
}

func dataplaneContainer(c Config) corev1.Container {
	diagnosticsPort := diagnosticsPort(c)

	return corev1.Container{
		Name:                     "dataplane",
		Image:                    c.DataplaneImage,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Env:                      dataplaneEnvironment(c, diagnosticsPort),
		Ports: []corev1.ContainerPort{
			{Name: "peer", ContainerPort: int32(c.PeerPort), Protocol: corev1.ProtocolTCP},
			{Name: "diagnostics", ContainerPort: diagnosticsPort, Protocol: corev1.ProtocolTCP},
		},
		// Readiness may wait on enrollment/recovery indefinitely without probe
		// restarts. Membership must continue to include unready Pods.
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
				Path: "/readyz", Port: intstr.FromString("diagnostics"), Scheme: corev1.URISchemeHTTP,
			}},
			PeriodSeconds: 5, TimeoutSeconds: 2, SuccessThreshold: 1, FailureThreshold: 1,
		},
		SecurityContext: &corev1.SecurityContext{
			// Native verbs require host device access. Privileged mode
			// implies escalation and all capabilities; do not claim otherwise.
			Privileged: ptr.To(true), AllowPrivilegeEscalation: ptr.To(true), ReadOnlyRootFilesystem: ptr.To(true),
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: "token", MountPath: "/var/run/racer-token", ReadOnly: true},
			{Name: "bootstrap", MountPath: "/etc/racer/bootstrap", ReadOnly: true},
			{Name: "identity", MountPath: "/var/lib/racer/identity"},
			{Name: "slabs", MountPath: "/var/lib/racer/slabs"},
			{Name: "sockets", MountPath: "/run/racer"},
			// Protect directory entries, not device I/O or the host
			// from this privileged container.
			{Name: "infiniband", MountPath: "/dev/infiniband", ReadOnly: true},
			{Name: "devices", MountPath: "/host/dev", ReadOnly: true},
		},
	}
}

func dataplaneEnvironment(c Config, diagnosticsPort int32) []corev1.EnvVar {
	return []corev1.EnvVar{
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
		{
			Name:  "RACER_DEVICE_DIRECTORY",
			Value: "/host/dev",
		},
	}
}

// DesiredDaemonSets builds steady-state placement, not a safe migration plan.
// The operator must additionally exclude occupied destination nodes until every
// source Pod, including terminating Pods, has disappeared.
func DesiredDaemonSets(c Config) ([]*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	nodes := slices.Clone(c.PodNetworkNodes)
	slices.Sort(nodes)

	c.PodNetworkNodes = nil

	host := buildDaemonSet(c)

	if len(nodes) == 0 {
		return []*appsv1.DaemonSet{host}, nil
	}

	c.HostNetwork = false
	c.DaemonSetName = PodNetworkDaemonSetName

	pod := buildDaemonSet(c)

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

// Observed workload ownership and endpoint selection.

// WorkloadIdentities is the configured live DaemonSet identity, not a label selector.
// Owns performs no I/O; callers refresh the snapshot with ReadWorkloadIdentities
// on every topology or authorization pass so ownership never relies on stale UIDs.
type WorkloadIdentities struct {
	Namespace string
	Name      string
	UID       types.UID
}

// Owns checks ownership only. Callers retain their Pod, Node, service-account,
// token and readiness-independent membership checks.
func (ids WorkloadIdentities) Owns(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}

	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return false
	}

	if pod.Namespace != ids.Namespace {
		return false
	}

	return owner.Name == ids.Name && owner.UID == ids.UID
}

// SelectEndpoint verifies workload ownership, ignores terminal/terminating/IP-less
// Pods, and chooses the newest creation time, breaking ties by UID. Readiness is ignored.
func SelectEndpoint(pods []corev1.Pod, ownership WorkloadIdentities, nodeName string, port uint16) (string, error) {
	if nodeName == "" || port == 0 {
		return "", wire.InvalidRequest
	}

	var (
		selected *corev1.Pod
		address  netip.Addr
	)

	for i := range pods {
		pod := &pods[i]

		ip, eligible := endpointAddress(pod, ownership, nodeName)
		if !eligible {
			continue
		}

		if selected == nil || pod.CreationTimestamp.After(selected.CreationTimestamp.Time) ||
			pod.CreationTimestamp.Equal(&selected.CreationTimestamp) && pod.UID > selected.UID {
			selected, address = pod, ip
		}
	}

	if selected == nil {
		return "", wire.Unavailable
	}

	return netip.AddrPortFrom(address, port).String(), nil
}

func endpointAddress(pod *corev1.Pod, ownership WorkloadIdentities, nodeName string) (netip.Addr, bool) {
	if pod.Spec.NodeName != nodeName || pod.DeletionTimestamp != nil || pod.UID == "" {
		return netip.Addr{}, false
	}

	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded || !ownership.Owns(pod) {
		return netip.Addr{}, false
	}

	ip, err := netip.ParseAddr(pod.Status.PodIP)

	return ip, err == nil && ip.Zone() == ""
}

// Membership candidates and recovery.

const (
	EnrolledSharesAnnotation   = "racer.unbounded-cloud.io/enrolled-shares"
	EnrolledRDMANICsAnnotation = "racer.unbounded-cloud.io/enrolled-rdma-nics"
	AdmittedMemberAnnotation   = "racer.unbounded-cloud.io/last-admitted-member"
)

// History contains only previously published members. Node annotations provide
// UID-bound recovery hints when an entry is absent. Callers must not advance this
// history until publication succeeds.
type History map[wire.NodeID]wire.Member

// Input is one observed topology snapshot. Pods are grouped by assigned Node
// name; each group is still checked for assignment and workload ownership.
// Reconcile borrows these values without mutating or retaining them.
type Input struct {
	Nodes      []corev1.Node
	PodsByNode map[string][]corev1.Pod
	Ownership  WorkloadIdentities
	PeerPort   uint16
}

// Result owns its members (including nested NICs) and diagnostics. Members is a
// candidate, not accepted state, until the caller successfully publishes it.
type Result struct {
	Members     History
	Diagnostics []Diagnostic
}

type MemberAttributes struct {
	Shares   uint32
	RDMANICs []wire.RDMANIC
}

type Diagnostic struct {
	Object string
	Field  string
	Reason string
}

// ParseAnnotations distinguishes absent defaults from malformed proposed updates.
func ParseAnnotations(node *corev1.Node) (MemberAttributes, error) {
	if node == nil {
		return MemberAttributes{}, wire.InvalidRequest
	}

	attributes := MemberAttributes{Shares: wire.DefaultShares, RDMANICs: []wire.RDMANIC{}}

	if _, explicit := node.Annotations[wire.SharesAnnotation]; !explicit {
		if value := node.Annotations[EnrolledSharesAnnotation]; value != "" {
			shares, err := strconv.ParseUint(value, 10, 32)
			if err != nil || shares == 0 {
				return MemberAttributes{}, wire.InvalidRequest
			}

			attributes.Shares = uint32(shares)
		}
	}

	if value, present := node.Annotations[wire.SharesAnnotation]; present {
		shares, err := strconv.ParseUint(value, 10, 32)
		if err != nil || shares == 0 || strings.HasPrefix(value, "+") {
			return MemberAttributes{}, fmt.Errorf("%s: %w", wire.SharesAnnotation, wire.InvalidRequest)
		}

		attributes.Shares = uint32(shares)
	}

	field := wire.RDMANICsAnnotation

	value, present := node.Annotations[field]
	if !present {
		field = EnrolledRDMANICsAnnotation
		value, present = node.Annotations[field]
	}

	if present {
		nics, err := wire.DecodeRDMANICs(strings.NewReader(value))
		if err != nil {
			return MemberAttributes{}, fmt.Errorf("%s: %w", field, err)
		}

		attributes.RDMANICs = nics
	}

	return attributes, nil
}

// Reconcile preserves admitted values across gaps using UID-bound Node
// annotations on restart. Never-admitted nodes with unavailable or malformed
// required inputs are omitted. Deletion and exclusion remove membership.
// Annotations are accepted as one unit, independently of the endpoint. Site is
// always derived from current labels, never from admitted history, so a
// malformed annotation cannot retain a removed or changed RDMA boundary.
// The caller installs returned history only after the candidate publication commits.
// Inputs and nested accepted state are never mutated or aliased by the result.
func Reconcile(input Input, accepted History) (Result, error) {
	if input.PeerPort == 0 {
		return Result{}, wire.InvalidRequest
	}

	nodes := slices.Clone(input.Nodes)
	slices.SortFunc(nodes, func(a, b corev1.Node) int { return cmp.Compare(a.UID, b.UID) })

	result := Result{Members: make(History), Diagnostics: []Diagnostic{}}
	ids, names := map[types.UID]bool{}, map[string]bool{}

	for i := range nodes {
		node := &nodes[i]
		if !wire.ValidUUID(string(node.UID)) || node.Name == "" || ids[node.UID] || names[node.Name] {
			return Result{}, fmt.Errorf("node identity: %w", wire.InvalidRequest)
		}

		ids[node.UID], names[node.Name] = true, true
		if _, excluded := node.Labels[wire.ExclusionLabel]; excluded {
			continue
		}

		result.reconcileNode(node, input, accepted)
	}

	if len(result.Members) > wire.MaxMembers {
		return Result{}, wire.TooLarge
	}

	return result, nil
}

func (result *Result) reconcileNode(node *corev1.Node, input Input, accepted History) {
	id := wire.NodeID(node.UID)

	previous, known := accepted[id]
	if !known {
		if saved, err := wire.DecodeAdmittedMember(strings.NewReader(node.Annotations[AdmittedMemberAnnotation])); err == nil && saved.Node == id {
			previous, known = saved, true
		}
	}

	attributes, annotationErr := ParseAnnotations(node)
	if annotationErr != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Object: node.Name, Field: "annotations", Reason: annotationErr.Error()})

		if known {
			attributes = MemberAttributes{Shares: previous.Shares, RDMANICs: previous.RDMANICs}
		}
	}
	// Node identity and peer port were validated by Reconcile, so the only
	// possible endpoint error here is Unavailable.
	endpoint, endpointErr := SelectEndpoint(input.PodsByNode[node.Name], input.Ownership, node.Name, input.PeerPort)
	if endpointErr != nil {
		result.Diagnostics = append(result.Diagnostics, Diagnostic{Object: node.Name, Field: "peer_endpoint", Reason: "no eligible managed Pod endpoint"})

		if known {
			endpoint = previous.PeerEndpoint
		}
	}

	if !known && (annotationErr != nil || endpointErr != nil) {
		return
	}

	result.Members[id] = wire.Member{
		Node: id, Shares: attributes.Shares, RDMANICs: wire.CanonicalRDMANICs(attributes.RDMANICs), PeerEndpoint: endpoint,
		// Current labels revoke a stale RDMA boundary even if annotations are invalid.
		Site: node.Labels[machinav1.MachineSiteLabelKey],
	}
}

// BuildCatalog selects Cache volumes, deriving identities from UIDs and paths from names, sorted by UID.
// An invalid catalog never partially replaces the currently served publication.
func BuildCatalog(volumes []racerv1.ClusterVolume) ([]wire.CacheDefinition, error) {
	catalog := make([]wire.CacheDefinition, 0, len(volumes))
	ids := make(map[wire.CacheID]bool, len(volumes))

	names := make(map[string]bool, len(volumes))
	for _, volume := range volumes {
		if volume.Spec.Type != racerv1.ClusterVolumeTypeCache {
			continue
		}

		id := wire.CacheID(volume.UID)
		if !wire.ValidUUID(string(id)) || ids[id] || names[volume.Name] {
			return nil, fmt.Errorf("cache identity: %w", wire.InvalidRequest)
		}

		client, origin, err := wire.CanonicalSocketPaths(volume.Name)
		if err != nil {
			return nil, fmt.Errorf("cache socket paths: %w", err)
		}

		ids[id], names[volume.Name] = true, true
		catalog = append(catalog, wire.CacheDefinition{ID: id, Name: volume.Name, ClientSocket: client, OriginSocket: origin})
	}

	slices.SortFunc(catalog, func(a, b wire.CacheDefinition) int { return cmp.Compare(a.ID, b.ID) })

	return catalog, nil
}
