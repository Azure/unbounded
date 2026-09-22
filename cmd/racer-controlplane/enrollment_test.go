// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer"
)

func enrollmentObjects() (*corev1.Pod, *appsv1.DaemonSet, *corev1.Node, *machina.Site) {
	site := &machina.Site{ObjectMeta: metav1.ObjectMeta{Name: "edge", UID: "site-uid"}}
	site.Spec.Components.Racer = &machina.RacerComponentSpec{SiteComponentSpec: machina.SiteComponentSpec{Enabled: ptr.To(true)}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: "node-uid", Labels: map[string]string{racer.SiteLabelKey: site.Name}}}
	daemon := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "racer-edge", UID: "daemon-uid", Labels: map[string]string{racer.MetadataPrefix + "component": "racer-dataplane"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: machina.GroupVersion.String(), Kind: "Site", Name: site.Name, UID: site.UID}}}}
	daemon.Spec.Template.Spec.ServiceAccountName = "racer-dataplane"
	daemon.Spec.Template.Labels = map[string]string{racer.UniverseKey: "edge"}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "system", Name: "racer-worker", UID: "pod-uid", Labels: map[string]string{racer.DataplaneLabelKey: "true", racer.UniverseKey: "edge"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: daemon.Name, UID: daemon.UID, Controller: ptr.To(true)}}}, Spec: corev1.PodSpec{ServiceAccountName: "racer-dataplane", NodeName: node.Name}}

	return pod, daemon, node, site
}

func TestEnrollmentHTTPContract(t *testing.T) {
	p, d, n, site := enrollmentObjects()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(p, d, n, site).Build()
	issued := 0
	server := &enrollmentServer{kube: kube, review: &reviewTestClient{review: validReview}, namespace: "system", issue: func(_ context.Context, csr string, id enrollmentIdentity) (enrollmentResponse, error) {
		issued++

		if csr != "node-csr" || id.podUID != "pod-uid" || id.boot != strings.Repeat("a", 64) {
			t.Fatalf("wrong server-derived request: %q %+v", csr, id)
		}

		return enrollmentResponse{Certificate: "leaf+chain", Generation: 7, Issuer: strings.Repeat("b", 64)}, nil
	}}

	for _, tc := range []struct {
		name, body, boot, token string
		code                    int
	}{
		{"success", `{"csr":"node-csr","pod_namespace":"system","pod_name":"racer-worker"}`, strings.Repeat("a", 64), "token", http.StatusOK},
		{"missing boot", `{"csr":"node-csr","pod_namespace":"system","pod_name":"racer-worker"}`, "", "token", http.StatusBadRequest},
		{"override identity", `{"csr":"node-csr","pod_namespace":"system","pod_name":"racer-worker","node":"forged"}`, strings.Repeat("a", 64), "token", http.StatusBadRequest},
		{"missing bearer", `{"csr":"node-csr","pod_namespace":"system","pod_name":"racer-worker"}`, strings.Repeat("a", 64), "", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "https://control/v3/enroll", strings.NewReader(tc.body))
			req.Header.Set("X-Racer-Boot", tc.boot)

			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}

			w := httptest.NewRecorder()
			server.enroll(w, req)

			if w.Code != tc.code {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}

			if tc.code == http.StatusOK {
				var response enrollmentResponse
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Generation != 7 || response.Certificate != "leaf+chain" {
					t.Fatalf("response=%+v err=%v", response, err)
				}
			}
		})
	}

	if issued != 1 {
		t.Fatalf("issued %d requests", issued)
	}
}

func TestEnrollmentKubernetesIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod, *appsv1.DaemonSet, *corev1.Node, *machina.Site)
		uid    string
		reject bool
	}{
		{name: "managed", uid: "pod-uid"},
		{name: "wrong token Pod", uid: "foreign", reject: true},
		{name: "wrong service account", uid: "pod-uid", reject: true, mutate: func(p *corev1.Pod, _ *appsv1.DaemonSet, _ *corev1.Node, _ *machina.Site) {
			p.Spec.ServiceAccountName = "default"
		}},
		{name: "forged owner UID", uid: "pod-uid", reject: true, mutate: func(p *corev1.Pod, _ *appsv1.DaemonSet, _ *corev1.Node, _ *machina.Site) {
			p.OwnerReferences[0].UID = "forged"
		}},
		{name: "unmanaged daemon", uid: "pod-uid", reject: true, mutate: func(_ *corev1.Pod, d *appsv1.DaemonSet, _ *corev1.Node, _ *machina.Site) { d.OwnerReferences = nil }},
		{name: "wrong Site", uid: "pod-uid", reject: true, mutate: func(p *corev1.Pod, _ *appsv1.DaemonSet, _ *corev1.Node, _ *machina.Site) {
			p.Labels[racer.UniverseKey] = "other"
		}},
		{name: "excluded Node", uid: "pod-uid", reject: true, mutate: func(_ *corev1.Pod, _ *appsv1.DaemonSet, n *corev1.Node, _ *machina.Site) {
			n.Labels[racer.ExcludeLabelKey] = "true"
		}},
		{name: "disabled Site", uid: "pod-uid", reject: true, mutate: func(_ *corev1.Pod, _ *appsv1.DaemonSet, _ *corev1.Node, s *machina.Site) {
			s.Spec.Components.Racer.Enabled = ptr.To(false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, d, n, s := enrollmentObjects()
			if tc.mutate != nil {
				tc.mutate(p, d, n, s)
			}

			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}

			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects([]client.Object{p, d, n, s}...).Build()

			id, err := enrollmentPodIdentity(context.Background(), kube, types.NamespacedName{Namespace: p.Namespace, Name: p.Name}, tc.uid)
			if tc.reject {
				if err == nil {
					t.Fatal("unauthorized Pod enrolled")
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if id.podUID != "pod-uid" || id.node != racer.Identity("node", "node-uid") || id.universe != racer.Identity("universe", "edge") {
				t.Fatalf("identity not server-derived: %+v", id)
			}
		})
	}
}

func TestControlRequiresExactVerifiedTLSIdentity(t *testing.T) {
	u, n := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tc := range []struct {
		name, uri                 string
		verified, expired, reject bool
	}{
		{"valid", "spiffe://racer/universe/" + u + "/node/" + n + "/pod/pod-uid", true, false, false},
		{"no verified chain", "spiffe://racer/universe/" + u + "/node/" + n + "/pod/pod-uid", false, false, true},
		{"other universe", "spiffe://racer/universe/" + n + "/node/" + n + "/pod/pod-uid", true, false, true},
		{"control plane", "spiffe://racer/controlplane", true, false, true},
		{"expired connection", "spiffe://racer/universe/" + u + "/node/" + n + "/pod/pod-uid", true, true, true},
		{"escaped Pod", "spiffe://racer/universe/" + u + "/node/" + n + "/pod/pod%2fuid", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri, err := url.Parse(tc.uri)
			if err != nil {
				t.Fatal(err)
			}

			leaf := &x509.Certificate{URIs: []*url.URL{uri}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
			if tc.expired {
				leaf.NotAfter = time.Now().Add(-time.Second)
			}

			req := httptest.NewRequest("GET", "https://control/v3/"+u+"/"+n, nil)
			req.SetPathValue("universe", u)
			req.SetPathValue("node", n)

			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
			if tc.verified {
				req.TLS.VerifiedChains = [][]*x509.Certificate{{leaf}}
			}

			uid, err := authenticateControl(req)
			if tc.reject {
				if err == nil {
					t.Fatal("unauthenticated control accepted")
				}

				return
			}

			if err != nil || uid != "pod-uid" {
				t.Fatalf("uid=%q err=%v", uid, err)
			}
		})
	}
}

func TestContainerRetirementRequiresTerminationEvidence(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "dataplane", ContainerID: "containerd://new", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if containerAuthoritativelyStopped(pod, "dataplane", "containerd://old") {
		t.Fatal("new container ID alone retired old boot")
	}

	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ContainerID: "containerd://different"}
	if containerAuthoritativelyStopped(pod, "dataplane", "containerd://old") {
		t.Fatal("unrelated termination retired old boot")
	}

	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ContainerID = "containerd://old"
	if !containerAuthoritativelyStopped(pod, "dataplane", "containerd://old") {
		t.Fatal("confirmed old container termination not recognized")
	}

	if containerAuthoritativelyStopped(pod, "dataplane", "") {
		t.Fatal("missing historical identity retired")
	}
}
