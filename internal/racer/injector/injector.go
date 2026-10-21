// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package injector implements a mutating admission handler that injects the
// racer-object sidecar into opted-in pods as a native (restartable init) sidecar.
package injector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/Azure/unbounded/internal/racer/wire"
	"github.com/Azure/unbounded/pkg/racersdk"
)

// SidecarUID is the UID and GID the injected sidecar runs as. The sidecar
// needs no root: Racer client sockets are mode 0666 and access is controlled
// by which hostPath directory the pod mounts.
const SidecarUID int64 = 65532

const (
	Path                    = "/mutate-racer-object-sidecar"
	ContainerName           = "racer-object"
	VolumeName              = "racer-object-client"
	EnabledLabel            = "racer.unbounded-cloud.io/object-sidecar"
	EnabledValue            = "enabled"
	CacheAnnotation         = "racer.unbounded-cloud.io/object-cache"
	APIAnnotation           = "racer.unbounded-cloud.io/object-api"
	ListenAnnotation        = "racer.unbounded-cloud.io/object-listen"
	CPURequestAnnotation    = "racer.unbounded-cloud.io/object-cpu-request"
	CPULimitAnnotation      = "racer.unbounded-cloud.io/object-cpu-limit"
	MemoryRequestAnnotation = "racer.unbounded-cloud.io/object-memory-request"
	MemoryLimitAnnotation   = "racer.unbounded-cloud.io/object-memory-limit"
	InjectedAnnotation      = "racer.unbounded-cloud.io/object-sidecar-injected"

	defaultCache = "racer-object"
	osLabel      = "kubernetes.io/os"
)

// removedAnnotations were used to scope the sidecar before each ClusterCache
// mapped to exactly one bucket.
var removedAnnotations = []string{
	"racer.unbounded-cloud.io/object-namespace",
	"racer.unbounded-cloud.io/object-buckets",
}

// Config configures the injected sidecar.
type Config struct {
	// Image is the racer-object container image.
	Image string
}

// Validate reports whether the configuration is usable.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Image) == "" {
		return errors.New("racer-object sidecar image must not be empty")
	}

	return nil
}

// Mutate injects the racer-object sidecar into pod in place. Pods without the
// opt-in label, or that already contain a container named ContainerName, are
// left unchanged. Invalid annotations return an error naming the annotation.
func Mutate(pod *corev1.Pod, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	if pod.Labels[EnabledLabel] != EnabledValue {
		return nil
	}

	for _, c := range pod.Spec.InitContainers {
		if c.Name == ContainerName {
			return nil
		}
	}

	for _, c := range pod.Spec.Containers {
		if c.Name == ContainerName {
			return nil
		}
	}

	// With host networking the loopback listener would be shared with every
	// hostNetwork workload on the node, and the sidecar has no per-client
	// authorization.
	if pod.Spec.HostNetwork {
		return errors.New("pod spec.hostNetwork is true, which the racer-object sidecar does not support because its loopback listener would be reachable from the node network namespace")
	}

	spec, err := parseAnnotations(pod.Annotations)
	if err != nil {
		return err
	}

	for _, v := range pod.Spec.Volumes {
		if v.Name == VolumeName {
			return fmt.Errorf("pod already defines a volume named %q", VolumeName)
		}
	}

	if pod.Spec.OS != nil && pod.Spec.OS.Name != corev1.Linux {
		return fmt.Errorf("pod spec.os.name %q conflicts with the racer-object sidecar, which requires linux", pod.Spec.OS.Name)
	}

	if v, ok := pod.Spec.NodeSelector[osLabel]; ok && v != "linux" {
		return fmt.Errorf("pod nodeSelector %s=%q conflicts with the racer-object sidecar, which requires linux", osLabel, v)
	}

	clientDir := "/run/racer/" + spec.cache + "/client"

	pod.Spec.InitContainers = append([]corev1.Container{{
		Name:          ContainerName,
		Image:         cfg.Image,
		Args:          spec.args(),
		RestartPolicy: ptr.To(corev1.ContainerRestartPolicyAlways),
		Resources:     spec.resources,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                ptr.To[int64](SidecarUID),
			RunAsGroup:               ptr.To[int64](SidecarUID),
			RunAsNonRoot:             ptr.To(true),
			AllowPrivilegeEscalation: ptr.To(false),
			ReadOnlyRootFilesystem:   ptr.To(true),
			Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: VolumeName, MountPath: clientDir, ReadOnly: true}},
	}}, pod.Spec.InitContainers...)

	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: VolumeName,
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
			Path: clientDir,
			Type: ptr.To(corev1.HostPathDirectory),
		}},
	})

	if pod.Spec.NodeSelector == nil {
		pod.Spec.NodeSelector = map[string]string{}
	}

	pod.Spec.NodeSelector[osLabel] = "linux"

	addExclusionAffinity(&pod.Spec)

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}

	pod.Annotations[InjectedAnnotation] = cfg.Image

	return nil
}

type sidecarSpec struct {
	cache     string
	api       string
	listen    string
	resources corev1.ResourceRequirements
}

func (s sidecarSpec) args() []string {
	// Kubelet expands $(VAR) in container args. Cache, api, and listen values
	// are validated and cannot contain "$", so no escaping is needed.
	args := []string{"sidecar", "--cache=" + s.cache}

	if s.api != "" {
		args = append(args, "--api="+s.api)
	}

	if s.listen != "" {
		args = append(args, "--listen="+s.listen)
	}

	return args
}

func parseAnnotations(a map[string]string) (sidecarSpec, error) {
	var s sidecarSpec

	// The sidecar serves the whole keyspace of its cache; the origin enforces
	// the bucket. Reject the removed scope annotations so stale manifests fail
	// loudly instead of silently widening access.
	for _, name := range removedAnnotations {
		if _, ok := a[name]; ok {
			return s, fmt.Errorf("annotation %s is no longer supported; the ClusterCache origin selects the bucket", name)
		}
	}

	s.cache = defaultCache

	if v, ok := a[CacheAnnotation]; ok {
		if err := validateCache(v); err != nil {
			return s, fmt.Errorf("annotation %s has invalid cache name %q", CacheAnnotation, v)
		}

		s.cache = v
	}

	if v, ok := a[APIAnnotation]; ok {
		if v != "s3" && v != "azure" {
			return s, fmt.Errorf("annotation %s must be s3 or azure", APIAnnotation)
		}

		s.api = v
	}

	if v, ok := a[ListenAnnotation]; ok {
		if !validListen(v) {
			return s, fmt.Errorf("annotation %s must be a loopback IP literal and port (1-65535)", ListenAnnotation)
		}

		s.listen = v
	}

	res, err := parseResources(a)
	if err != nil {
		return s, err
	}

	s.resources = res

	return s, nil
}

// validateCache uses the same validation as the racer-object binary, which
// rejects anything that is not a lowercase DNS-style name (no slashes or "..").
func validateCache(cache string) error {
	if cache == "" || strings.Contains(cache, "/") || strings.Contains(cache, "..") {
		return errors.New("invalid cache name")
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cache})
	if err != nil {
		return err
	}

	return client.Close()
}

func validListen(value string) bool {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return false
	}

	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return false
	}

	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return false
	}

	n, err := strconv.Atoi(port)

	return err == nil && n > 0 && n <= 65535
}

func parseResources(a map[string]string) (corev1.ResourceRequirements, error) {
	var r corev1.ResourceRequirements

	type pair struct {
		name             corev1.ResourceName
		reqAnno, limAnno string
	}

	for _, p := range []pair{
		{corev1.ResourceCPU, CPURequestAnnotation, CPULimitAnnotation},
		{corev1.ResourceMemory, MemoryRequestAnnotation, MemoryLimitAnnotation},
	} {
		req, hasReq, err := parseQuantity(a, p.reqAnno)
		if err != nil {
			return r, err
		}

		lim, hasLim, err := parseQuantity(a, p.limAnno)
		if err != nil {
			return r, err
		}

		if hasReq && hasLim && req.Cmp(lim) > 0 {
			return r, fmt.Errorf("annotation %s must not exceed %s", p.reqAnno, p.limAnno)
		}

		if hasReq {
			if r.Requests == nil {
				r.Requests = corev1.ResourceList{}
			}

			r.Requests[p.name] = req
		}

		if hasLim {
			if r.Limits == nil {
				r.Limits = corev1.ResourceList{}
			}

			r.Limits[p.name] = lim
		}
	}

	return r, nil
}

func parseQuantity(a map[string]string, key string) (resource.Quantity, bool, error) {
	v, ok := a[key]
	if !ok {
		return resource.Quantity{}, false, nil
	}

	q, err := resource.ParseQuantity(strings.TrimSpace(v))
	if err != nil || q.Sign() <= 0 {
		return resource.Quantity{}, false, fmt.Errorf("annotation %s must be a positive resource quantity", key)
	}

	return q, true, nil
}

func exclusionRequirement() corev1.NodeSelectorRequirement {
	return corev1.NodeSelectorRequirement{Key: wire.ExclusionLabel, Operator: corev1.NodeSelectorOpDoesNotExist}
}

func addExclusionAffinity(spec *corev1.PodSpec) {
	if spec.Affinity == nil {
		spec.Affinity = &corev1.Affinity{}
	}

	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &corev1.NodeAffinity{}
	}

	na := spec.Affinity.NodeAffinity
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{exclusionRequirement()},
			}},
		}

		return
	}

	// An existing selector with no terms, and any empty term, match no
	// nodes. Leave them unchanged: adding the exclusion would make them
	// match every node without the exclusion label.
	sel := na.RequiredDuringSchedulingIgnoredDuringExecution
	want := exclusionRequirement()

	for i := range sel.NodeSelectorTerms {
		term := &sel.NodeSelectorTerms[i]
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}

		present := false

		for _, e := range term.MatchExpressions {
			if e.Key == want.Key && e.Operator == want.Operator && len(e.Values) == 0 {
				present = true
				break
			}
		}

		if !present {
			term.MatchExpressions = append(term.MatchExpressions, want)
		}
	}
}

type handler struct {
	cfg Config
}

// NewHandler returns an admission handler that injects the sidecar into
// created v1 Pods.
func NewHandler(cfg Config) admission.Handler {
	return &handler{cfg: cfg}
}

func (h *handler) Handle(_ context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create || req.Kind.Group != "" || req.Kind.Version != "v1" || req.Kind.Kind != "Pod" {
		return admission.Allowed("")
	}

	pod := &corev1.Pod{}
	if err := json.Unmarshal(req.Object.Raw, pod); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	if err := Mutate(pod, h.cfg); err != nil {
		return admission.Denied(err.Error())
	}

	mutated, err := json.Marshal(pod)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}

	return admission.PatchResponseFromRaw(req.Object.Raw, mutated)
}
