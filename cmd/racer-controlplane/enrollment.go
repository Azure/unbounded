// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/pki"
)

type enrollmentIdentity struct {
	universe, node, podUID, boot, containerID string
	podName                                   string
}

// Existing draining participants retain their originally admitted identity when
// their Site is disabled or Node is excluded or replaced. A new process cannot use this path.
func retainedEnrollmentIdentity(ctx context.Context, kube client.Reader, ca *pki.Manager, key types.NamespacedName, uid, boot string) (enrollmentIdentity, error) {
	var pod corev1.Pod
	if err := kube.Get(ctx, key, &pod); err != nil {
		return enrollmentIdentity{}, err
	}

	if string(pod.UID) != uid || pod.Spec.ServiceAccountName != "racer-dataplane" {
		return enrollmentIdentity{}, errInvalidCredential
	}

	member, err := ca.Member(ctx, pki.MemberKey{PodUID: uid, BootID: boot})
	if err != nil {
		return enrollmentIdentity{}, err
	}

	if member.Kind == pki.Node {
		return enrollmentIdentity{universe: member.Universe, node: member.Node, podUID: uid, boot: boot, containerID: member.ContainerID, podName: pod.Name}, nil
	}

	return enrollmentIdentity{}, errInvalidCredential
}

type enrollmentResponse struct {
	Certificate string `json:"certificate"`
	Generation  uint64 `json:"generation"`
	Issuer      string `json:"issuer"`
}

type enrollmentServer struct {
	inflight    atomic.Int32
	kube        client.Client
	review      client.Client
	namespace   string
	credentials credentialCache
	issue       func(context.Context, string, enrollmentIdentity) (enrollmentResponse, error)
	selected    func(enrollmentIdentity) bool
	renewal     func(context.Context, types.NamespacedName, string, string) (enrollmentIdentity, error)
}

// Issuance commits are serialized. Bound the entire enrollment pipeline before
// TokenReview and live ownership reads so a fleet retry wave cannot overwhelm
// the API while waiting for that single writer. Leave time for the response
// within the dataplane's five-second read timeout.
const (
	enrollmentConcurrency = 8
	enrollmentTimeout     = 4 * time.Second
)

func (s *enrollmentServer) enroll(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if req.TLS == nil {
		http.Error(w, "TLS required", http.StatusForbidden)
		return
	}

	if s.inflight.Add(1) > enrollmentConcurrency {
		s.inflight.Add(-1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "enrollment busy", http.StatusServiceUnavailable)

		return
	}
	defer s.inflight.Add(-1)

	ctx, cancel := context.WithTimeout(req.Context(), enrollmentTimeout)
	defer cancel()

	req = req.WithContext(ctx)

	var body struct {
		CSR       string `json:"csr"`
		Namespace string `json:"pod_namespace"`
		Name      string `json:"pod_name"`
	}

	decoder := json.NewDecoder(http.MaxBytesReader(w, req.Body, 32768))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&body); err != nil {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}

	if decoder.Decode(new(any)) != io.EOF || body.CSR == "" || body.Namespace != s.namespace || body.Name == "" {
		http.Error(w, "invalid enrollment request", http.StatusBadRequest)
		return
	}

	token, bearer := strings.CutPrefix(req.Header.Get("Authorization"), "Bearer ")
	if !bearer {
		http.Error(w, "Pod credential required", http.StatusForbidden)
		return
	}

	uid, err := s.credentials.authenticate(req.Context(), s.review, token, controlAudience)
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, errInvalidCredential) {
			code = http.StatusForbidden
		}

		http.Error(w, "Pod credential rejected", code)

		return
	}

	boot, err := hex.DecodeString(req.Header.Get("X-Racer-Boot"))
	if err != nil || len(boot) != 32 {
		http.Error(w, "X-Racer-Boot must contain a 32-byte process nonce", http.StatusBadRequest)
		return
	}

	key := types.NamespacedName{Namespace: body.Namespace, Name: body.Name}

	var id enrollmentIdentity

	// Prefer the durable identity for an existing boot, even while it remains
	// selected. Pod status can still describe the preceding container when a
	// replacement process enrolls; a later status update must not change its
	// admission metadata. This also preserves a draining Node's original identity.
	err = errInvalidCredential
	if s.renewal != nil {
		id, err = s.renewal(req.Context(), key, uid, hex.EncodeToString(boot))
	}

	if err != nil {
		id, err = enrollmentPodIdentity(req.Context(), s.kube, key, uid)
	}

	if err != nil {
		http.Error(w, "Pod is not eligible for enrollment", http.StatusForbidden)
		return
	}

	id.boot = hex.EncodeToString(boot)
	if s.selected != nil && !s.selected(id) {
		http.Error(w, "Pod identity is not admitted by committed topology", http.StatusForbidden)
		return
	}

	response, err := s.issue(req.Context(), body.CSR, id)
	if err != nil {
		http.Error(w, "certificate issuance unavailable", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("write enrollment response: %v", err)
	}
}

// All objects are read directly from the API. Labels alone never authorize a key.
func enrollmentPodIdentity(ctx context.Context, kube client.Reader, key types.NamespacedName, uid string) (enrollmentIdentity, error) {
	var pod corev1.Pod
	if err := kube.Get(ctx, key, &pod); err != nil {
		return enrollmentIdentity{}, err
	}

	if string(pod.UID) != uid || uid == "" || pod.DeletionTimestamp != nil || pod.Spec.ServiceAccountName != "racer-dataplane" || pod.Spec.NodeName == "" || pod.Labels[racer.DataplaneLabelKey] != "true" {
		return enrollmentIdentity{}, errInvalidCredential
	}

	owner := metav1.GetControllerOf(&pod)
	if owner == nil || owner.APIVersion != "apps/v1" || owner.Kind != "DaemonSet" || owner.UID == "" {
		return enrollmentIdentity{}, errInvalidCredential
	}

	var daemon appsv1.DaemonSet
	if err := kube.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: owner.Name}, &daemon); err != nil {
		return enrollmentIdentity{}, err
	}

	if daemon.UID != owner.UID || daemon.DeletionTimestamp != nil || daemon.Labels[racer.MetadataPrefix+"component"] != "racer-dataplane" || daemon.Spec.Template.Spec.ServiceAccountName != "racer-dataplane" {
		return enrollmentIdentity{}, errInvalidCredential
	}

	var node corev1.Node
	if err := kube.Get(ctx, types.NamespacedName{Name: pod.Spec.NodeName}, &node); err != nil {
		return enrollmentIdentity{}, err
	}

	if err := racer.ValidateBootstrapNode(&node, pod.Labels[racer.UniverseKey]); err != nil {
		return enrollmentIdentity{}, err
	}

	var site machina.Site
	if err := kube.Get(ctx, types.NamespacedName{Name: racer.NodeSite(&node)}, &site); err != nil {
		return enrollmentIdentity{}, err
	}

	if site.DeletionTimestamp != nil || site.Spec.Components.Racer == nil || !machina.ComponentEnabled(&site.Spec.Components.Racer.SiteComponentSpec) {
		return enrollmentIdentity{}, errInvalidCredential
	}

	siteOwned := false

	for _, ref := range daemon.OwnerReferences {
		if ref.APIVersion == machina.GroupVersion.String() && ref.Kind == "Site" && ref.Name == site.Name && ref.UID == site.UID && site.UID != "" {
			siteOwned = true
		}
	}

	if !siteOwned || daemon.Spec.Template.Labels[racer.UniverseKey] != racer.NodeUniverse(&node) {
		return enrollmentIdentity{}, errInvalidCredential
	}

	return enrollmentIdentity{universe: racer.Identity("universe", racer.NodeUniverse(&node)), node: racer.Identity("node", string(node.UID)), podUID: uid, containerID: runningContainerID(&pod, "dataplane"), podName: pod.Name}, nil
}

func runningContainerID(pod *corev1.Pod, name string) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == name && status.State.Running != nil {
			return status.ContainerID
		}
	}

	return ""
}

func containerAuthoritativelyStopped(pod *corev1.Pod, name, old string) bool {
	if old == "" {
		return false
	}

	for _, status := range pod.Status.ContainerStatuses {
		terminated := status.LastTerminationState.Terminated
		if status.Name == name && status.State.Running != nil && status.ContainerID != "" && status.ContainerID != old && terminated != nil && terminated.ContainerID == old {
			return true
		}
	}

	return false
}

func authenticateControl(req *http.Request) (string, error) {
	if req.TLS == nil || len(req.TLS.VerifiedChains) == 0 || len(req.TLS.VerifiedChains[0]) == 0 {
		return "", errInvalidCredential
	}

	leaf := req.TLS.VerifiedChains[0][0]
	if len(leaf.URIs) != 1 || time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return "", errInvalidCredential
	}

	u := leaf.URIs[0]

	prefix := "/universe/" + req.PathValue("universe") + "/node/" + req.PathValue("node") + "/pod/"
	if u.Scheme != "spiffe" || u.Host != "racer" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, prefix) {
		return "", errInvalidCredential
	}

	uid := strings.TrimPrefix(u.Path, prefix)
	if uid == "" || len(uid) > 256 || strings.ContainsAny(uid, "/%?#") {
		return "", fmt.Errorf("invalid certificate Pod identity")
	}

	return uid, nil
}
