// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package workload builds the Racer dataplane DaemonSet. The Unbounded
// operator deploys it. Each dataplane Pod also runs the Racer control plane,
// so there is no separate controller workload.
package workload

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
)

const (
	// DataplaneDaemonSetName names the dataplane DaemonSet and its ServiceAccount.
	DataplaneDaemonSetName = "racer"

	// ContainerName names the dataplane container.
	ContainerName = "dataplane"

	// ExclusionLabel removes a Node from dataplane placement and membership.
	ExclusionLabel = "racer.unbounded-cloud.io/exclude"

	// ControlTokenAudience is the audience of the token a Pod presents to
	// control seats.
	ControlTokenAudience = "racer-control"

	// ControlPort serves the control API on seat holders.
	ControlPort = 8443

	// WebhookPort serves the object sidecar admission webhook on seat holders.
	WebhookPort = 9443

	// ControlSeatsKey is the administrator seat policy, read from
	// racer-settings: auto picks 1 to 16 seats, or a fixed count of
	// 1 to 16. It is never injected as explicit env so the ConfigMap wins.
	ControlSeatsKey = "RACER_CONTROL_SEATS"
	// MaxControlSeats bounds a fixed seat count.
	MaxControlSeats = 16

	// APITokenPath is where the default-audience API credentials are
	// projected, the standard in-cluster client location.
	APITokenPath = "/var/run/secrets/kubernetes.io/serviceaccount"
)

// NVIDIAInjection selects how the dataplane asks the container runtime for
// the host NVIDIA driver libraries. No mode requests nvidia.com/gpu.
type NVIDIAInjection string

const (
	// NVIDIAOff adds no NVIDIA env, CDI annotation, /run/udev mount, or cuFile log volume.
	NVIDIAOff NVIDIAInjection = "off"
	// NVIDIAEnv sets NVIDIA_VISIBLE_DEVICES and NVIDIA_DRIVER_CAPABILITIES and
	// mounts /run/udev and a cuFile log emptyDir for cuFile.
	// The NVIDIA container runtime acts on the variables; runc ignores them, so they
	// are harmless on nodes without NVIDIA.
	NVIDIAEnv NVIDIAInjection = "env"
	// NVIDIACDI also adds the CDI pod annotation. containerd fails container
	// creation when a CDI device cannot be resolved, so use this only when
	// every dataplane node has an NVIDIA CDI spec.
	NVIDIACDI NVIDIAInjection = "cdi"
)

const (
	// NVIDIAKey selects the NVIDIAInjection mode, read from
	// racer-settings by the operator. Unset selects NVIDIAEnv.
	NVIDIAKey = "RACER_NVIDIA"
	// CDIAnnotation requests the NVIDIA CDI device on the dataplane Pod.
	// containerd treats every key with the cdi.k8s.io/ prefix as a CDI request.
	CDIAnnotation = "cdi.k8s.io/racer"
	// CDIAllGPUs is the fully qualified CDI name for every GPU on the node.
	CDIAllGPUs = "nvidia.com/gpu=all"
)

const (
	// CUFileLogDirectory is the logging.dir of the cufile.json in the
	// dataplane image (images/racer/cufile.json).
	CUFileLogDirectory = "/var/log/cufile"
	// CUFileLogSizeLimit bounds the cuFile log emptyDir. cuFile logs at
	// ERROR level only, so this is far above normal use.
	CUFileLogSizeLimit = "64Mi"
)

// ErrInvalid reports an invalid workload configuration.
var ErrInvalid = errors.New("invalid racer dataplane configuration")

// Config holds the inputs of the dataplane DaemonSet.
type Config struct {
	Namespace      string
	DataplaneImage string
	// ObjectImage is the racer-object sidecar image the webhook injects.
	ObjectImage string
	PeerPort    uint16
	HostNetwork bool
	// Zero selects 9090, or 9091 when the peer port is 9090.
	DiagnosticsPort uint16
	// Empty selects NVIDIAEnv.
	NVIDIA NVIDIAInjection
}

// DefaultConfig returns the managed defaults for a namespace and images.
func DefaultConfig(namespace, dataplaneImage, objectImage string) Config {
	return Config{Namespace: namespace, DataplaneImage: dataplaneImage, ObjectImage: objectImage, PeerPort: 8082}
}

// Validate checks names, images, and that every listener port is distinct.
func (c Config) Validate() error {
	if len(validation.IsDNS1123Label(c.Namespace)) != 0 {
		return fmt.Errorf("namespace %q: %w", c.Namespace, ErrInvalid)
	}

	if strings.TrimSpace(c.DataplaneImage) == "" || strings.TrimSpace(c.ObjectImage) == "" {
		return fmt.Errorf("dataplane and object images are required: %w", ErrInvalid)
	}

	if c.PeerPort < 1024 || (c.DiagnosticsPort != 0 && c.DiagnosticsPort < 1024) {
		return fmt.Errorf("ports must be 1024..65535: %w", ErrInvalid)
	}

	if mode := c.nvidiaMode(); mode != NVIDIAOff && mode != NVIDIAEnv && mode != NVIDIACDI {
		return fmt.Errorf("NVIDIA injection must be off, env, or cdi: %w", ErrInvalid)
	}

	ports := map[int32]bool{}
	for _, port := range []int32{int32(c.PeerPort), c.diagnosticsPort(), ControlPort, WebhookPort} {
		if ports[port] {
			return fmt.Errorf("peer, diagnostics, control, and webhook ports must be distinct: %w", ErrInvalid)
		}

		ports[port] = true
	}

	return nil
}

func (c Config) nvidiaMode() NVIDIAInjection {
	if c.NVIDIA == "" {
		return NVIDIAEnv
	}

	return c.NVIDIA
}

func (c Config) diagnosticsPort() int32 {
	if c.DiagnosticsPort != 0 {
		return int32(c.DiagnosticsPort)
	}

	if c.PeerPort == 9090 {
		return 9091
	}

	return 9090
}

// Labels are the immutable selector labels of the dataplane Pods.
func Labels() map[string]string {
	return map[string]string{"app.kubernetes.io/name": DataplaneDaemonSetName, "app.kubernetes.io/instance": DataplaneDaemonSetName}
}

// DaemonSet builds the dataplane DaemonSet.
func DaemonSet(c Config) (*appsv1.DaemonSet, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}

	var annotations map[string]string
	if c.nvidiaMode() == NVIDIACDI {
		annotations = map[string]string{CDIAnnotation: CDIAllGPUs}
	}

	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: DataplaneDaemonSetName, Namespace: c.Namespace, Labels: Labels()},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: Labels()},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
					MaxSurge:       ptr.To(intstr.FromInt32(0)),
				},
			},
			// Require sustained readiness across probe periods before advancing a rollout.
			MinReadySeconds: 10,
			Template:        corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: Labels(), Annotations: annotations}, Spec: dataplanePod(c)},
		},
	}, nil
}

func dataplanePod(c Config) corev1.PodSpec {
	pod := corev1.PodSpec{
		ServiceAccountName: DataplaneDaemonSetName,
		// The API credentials are projected explicitly instead, see apiVolume.
		AutomountServiceAccountToken:  ptr.To(false),
		RestartPolicy:                 corev1.RestartPolicyAlways,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 corev1.DefaultSchedulerName,
		EnableServiceLinks:            ptr.To(false),
		PreemptionPolicy:              ptr.To(corev1.PreemptLowerPriority),
		TerminationGracePeriodSeconds: ptr.To(int64(30)),
		SecurityContext:               &corev1.PodSecurityContext{RunAsUser: ptr.To(int64(0))},
		Affinity:                      DataplaneAffinity(),
		Containers:                    []corev1.Container{dataplaneContainer(c)},
		Volumes:                       dataplaneVolumes(c),
	}
	if c.HostNetwork {
		pod.HostNetwork = true
		pod.DNSPolicy = corev1.DNSClusterFirstWithHostNet
	}

	return pod
}

// DataplaneAffinity is the default node affinity of dataplane Pods. Workloads
// that must share nodes with the dataplane reuse it.
func DataplaneAffinity() *corev1.Affinity {
	return &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{
						{Key: ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist},
						{Key: "kubernetes.io/os", Operator: corev1.NodeSelectorOpIn, Values: []string{"linux"}},
					},
				}},
			},
		},
	}
}

// apiVolume replaces the automounted credentials with the same three files:
// a default-audience token, the namespace, and the cluster CA. Mounting them
// explicitly keeps the API token separate from the control token volume.
func apiVolume() corev1.Volume {
	return corev1.Volume{
		Name: "kube-api-access",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
			DefaultMode: ptr.To(int32(0o444)),
			Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: ptr.To(int64(3607)), Path: "token"}},
				{ConfigMap: &corev1.ConfigMapProjection{
					LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items:                []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}},
				}},
				{DownwardAPI: &corev1.DownwardAPIProjection{Items: []corev1.DownwardAPIVolumeFile{{
					Path: "namespace", FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: "metadata.namespace"},
				}}}},
			},
		}},
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
		apiVolume(),
		{
			Name: "token",
			VolumeSource: corev1.VolumeSource{
				Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: ptr.To(int32(0o400)),
					Sources: []corev1.VolumeProjection{{
						ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
							Audience:          ControlTokenAudience,
							ExpirationSeconds: ptr.To(int64(3600)),
							Path:              "token",
						},
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

	// cuFile reads the filesystem type of each slab device from the udev
	// database and refuses files it cannot classify.
	if c.nvidiaMode() != NVIDIAOff {
		volumes = append(volumes, corev1.Volume{Name: "udev", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/run/udev", Type: ptr.To(corev1.HostPathDirectoryOrCreate)}}})
		// The root filesystem is read-only, so cuFile writes cufile.log here.
		volumes = append(volumes, corev1.Volume{Name: "cufile-log", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse(CUFileLogSizeLimit))}}})
	}

	return volumes
}

func dataplaneContainer(c Config) corev1.Container {
	mounts := []corev1.VolumeMount{
		{Name: "kube-api-access", MountPath: APITokenPath, ReadOnly: true},
		{Name: "token", MountPath: "/var/run/racer-token", ReadOnly: true},
		{Name: "identity", MountPath: "/var/lib/racer/identity"},
		{Name: "slabs", MountPath: "/var/lib/racer/slabs"},
		{Name: "sockets", MountPath: "/run/racer"},
		// Protect directory entries, not device I/O or the host
		// from this privileged container.
		{Name: "infiniband", MountPath: "/dev/infiniband", ReadOnly: true},
		{Name: "devices", MountPath: "/host/dev", ReadOnly: true},
	}
	if c.nvidiaMode() != NVIDIAOff {
		mounts = append(mounts, corev1.VolumeMount{Name: "udev", MountPath: "/run/udev", ReadOnly: true}, corev1.VolumeMount{Name: "cufile-log", MountPath: CUFileLogDirectory})
	}

	return corev1.Container{
		Name:                     ContainerName,
		Image:                    c.DataplaneImage,
		ImagePullPolicy:          corev1.PullIfNotPresent,
		TerminationMessagePath:   corev1.TerminationMessagePathDefault,
		TerminationMessagePolicy: corev1.TerminationMessageReadFile,
		Env:                      dataplaneEnvironment(c),
		Ports: []corev1.ContainerPort{
			{Name: "peer", ContainerPort: int32(c.PeerPort), Protocol: corev1.ProtocolTCP},
			{Name: "diagnostics", ContainerPort: c.diagnosticsPort(), Protocol: corev1.ProtocolTCP},
			{Name: "control", ContainerPort: ControlPort, Protocol: corev1.ProtocolTCP},
			{Name: "webhook", ContainerPort: WebhookPort, Protocol: corev1.ProtocolTCP},
		},
		// Readiness may wait on enrollment/recovery indefinitely without probe
		// restarts. Membership must continue to include unready Pods.
		Resources: dataplaneResources(),
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
		VolumeMounts: mounts,
	}
}

func fieldEnv(name, path string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{APIVersion: "v1", FieldPath: path}}}
}

func dataplaneEnvironment(c Config) []corev1.EnvVar {
	listen := func(port int32) string { return "[$(RACER_POD_IP)]:" + strconv.Itoa(int(port)) }

	env := []corev1.EnvVar{
		fieldEnv("RACER_NAMESPACE", "metadata.namespace"),
		fieldEnv("RACER_POD_NAME", "metadata.name"),
		fieldEnv("RACER_POD_UID", "metadata.uid"),
		fieldEnv("RACER_NODE_NAME", "spec.nodeName"),
		// This is only a bind address, never an authority for node identity.
		// Define it before the listeners so kubelet expands either IP family.
		fieldEnv("RACER_POD_IP", "status.podIP"),
		// Every Pod IP. The webhook also serves the other family, since the
		// racer-webhook Service primary family need not match status.podIP.
		fieldEnv("RACER_POD_IPS", "status.podIPs"),
		{Name: "RACER_PEER_LISTEN", Value: listen(int32(c.PeerPort))},
		{Name: "RACER_DIAGNOSTICS_LISTEN", Value: listen(c.diagnosticsPort())},
		{Name: "RACER_CONTROL_LISTEN", Value: listen(ControlPort)},
		{Name: "RACER_WEBHOOK_LISTEN", Value: listen(WebhookPort)},
		{Name: "RACER_OBJECT_IMAGE", Value: c.ObjectImage},
		{Name: "RACER_SERVICE_ACCOUNT_TOKEN", Value: "/var/run/racer-token/token"},
		// Kubelet creates the hostPath mount with mode 0755. Let the
		// dataplane create its private 0700 directory beneath it.
		{Name: "RACER_IDENTITY_DIRECTORY", Value: "/var/lib/racer/identity/private"},
		{Name: "RACER_SLAB_DIRECTORY", Value: "/var/lib/racer/slabs"},
		{Name: "RACER_DEVICE_DIRECTORY", Value: "/host/dev"},
	}

	// Ask the NVIDIA runtime for driver libraries only. The privileged
	// container already sees /dev/nvidia*, including the GPUDirect Storage
	// /dev/nvidia-fs* nodes that exist when the Pod starts, so NVIDIA_GDS is
	// not needed. No GPU resource is requested.
	if c.nvidiaMode() != NVIDIAOff {
		env = append(env,
			corev1.EnvVar{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
			corev1.EnvVar{Name: "NVIDIA_DRIVER_CAPABILITIES", Value: "compute,utility"},
		)
	}

	return env
}

// DataplaneMemoryRequest is the default memory request for the dataplane
// container. It covers the fixed overhead (a checkpoint working set of up to
// 64 MiB by default, metadata, and runtime) plus the admission floors of a
// handful of I/O workers (roughly 115 MiB of byte budgets per worker), so the
// scheduler reserves a realistic baseline on every node.
const DataplaneMemoryRequest = "1Gi"

// dataplaneResources requests memory but sets no limit. Admission budgets
// scale with the node's I/O worker count, so any fixed limit would be too
// small on large nodes and risk OOM kills. Operators can add a limit with a
// DaemonSet override.
func dataplaneResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(DataplaneMemoryRequest)},
	}
}
