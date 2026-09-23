// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer/pki"
)

type failedRetirementList struct{ client.Client }

func (failedRetirementList) List(context.Context, client.ObjectList, ...client.ListOption) error {
	return errors.New("API unavailable")
}

func TestNodeEnrollmentSurvivesContainerStatusCatchup(t *testing.T) {
	for _, initialID := range []string{"", "containerd://preceding"} {
		t.Run(initialID, func(t *testing.T) {
			ctx := t.Context()
			pod, daemon, node, site := enrollmentObjects()
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "dataplane", ContainerID: initialID, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}

			scheme := runtime.NewScheme()
			for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
				if err := add(scheme); err != nil {
					t.Fatal(err)
				}
			}

			kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, daemon, node, site).Build()

			manager, err := pki.New(kube, pod.Namespace, pki.Options{})
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.AcquireLeadership(ctx, "leader"); err != nil {
				t.Fatal(err)
			}

			if err := manager.Publish(ctx); err != nil {
				t.Fatal(err)
			}

			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}

			der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
			if err != nil {
				t.Fatal(err)
			}

			csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
			server := &enrollmentServer{
				kube: kube, review: &reviewTestClient{review: validReview}, namespace: pod.Namespace,
				selected: func(id enrollmentIdentity) bool { return id.podUID == string(pod.UID) },
				renewal: func(ctx context.Context, key types.NamespacedName, uid, boot string) (enrollmentIdentity, error) {
					return retainedEnrollmentIdentity(ctx, kube, manager, key, uid, boot)
				},
				issue: func(ctx context.Context, _ string, id enrollmentIdentity) (enrollmentResponse, error) {
					issued, err := manager.Issue(ctx, csr, pki.Identity{Kind: pki.Node, Universe: id.universe, Node: id.node, PodUID: id.podUID, BootID: id.boot, ContainerID: id.containerID, PodName: id.podName})
					return enrollmentResponse{Certificate: string(issued.CertificatePEM)}, err
				},
			}
			boot := strings.Repeat("a", 64)
			enroll := func(want int) {
				t.Helper()

				req := httptest.NewRequest(http.MethodPost, "https://control/v3/enroll", strings.NewReader(`{"csr":"fixture-csr","pod_namespace":"system","pod_name":"racer-worker"}`))
				req.Header.Set("Authorization", "Bearer token")
				req.Header.Set("X-Racer-Boot", boot)

				w := httptest.NewRecorder()
				server.enroll(w, req)

				if w.Code != want {
					t.Fatalf("enrollment status=%d want=%d body=%s", w.Code, want, w.Body.String())
				}
			}
			// The new process enrolls while status still describes its predecessor.
			enroll(http.StatusOK)

			memberKey := pki.MemberKey{PodUID: string(pod.UID), BootID: boot}

			admitted, err := manager.Member(ctx, memberKey)
			if err != nil || admitted.ContainerID != initialID {
				t.Fatalf("initial admission=%+v error=%v", admitted, err)
			}

			pod.Status.ContainerStatuses[0].ContainerID = "containerd://current"

			pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ContainerID: "containerd://preceding"}
			if err := kube.Status().Update(ctx, pod); err != nil {
				t.Fatal(err)
			}

			control := &tlsControl{kube: kube, namespace: pod.Namespace, manager: manager}
			if err := control.retireDeletedNodes(ctx); err != nil {
				t.Fatal(err)
			}

			if got, err := manager.Member(ctx, memberKey); err != nil || got != admitted {
				t.Fatalf("status catchup retired or changed current boot: %+v %v", got, err)
			}

			enroll(http.StatusOK)

			if got, err := manager.Member(ctx, memberKey); err != nil || got != admitted {
				t.Fatalf("renewal changed admission: %+v %v", got, err)
			}
			// A prior buggy retirement is not repairable by replaying that boot,
			// even after collection or leader takeover. A fresh boot can enroll.
			if err := manager.Retire(ctx, memberKey); err != nil {
				t.Fatal(err)
			}

			if err := manager.CollectRetirements(ctx); err != nil {
				t.Fatal(err)
			}

			manager, err = pki.New(kube, pod.Namespace, pki.Options{})
			if err != nil {
				t.Fatal(err)
			}

			if err := manager.AcquireLeadership(ctx, "next-leader"); err != nil {
				t.Fatal(err)
			}

			enroll(http.StatusServiceUnavailable)

			boot = strings.Repeat("b", 64)

			enroll(http.StatusOK)
			// Pod replacement supplies authoritative retirement for all its boots.
			if err := kube.Delete(ctx, pod); err != nil {
				t.Fatal(err)
			}

			control.manager = manager
			if err := control.retireDeletedNodes(ctx); err != nil {
				t.Fatal(err)
			}

			if _, err := manager.Member(ctx, pki.MemberKey{PodUID: string(pod.UID), BootID: boot}); err == nil {
				t.Fatal("absent Pod retained its member")
			}

			if err := manager.Admit(ctx, admitted); err == nil {
				t.Fatal("retired boot re-admitted")
			}
		})
	}
}

func TestNodeRetirementRequiresAuthoritativePodAbsence(t *testing.T) {
	for _, mode := range []string{"present", "terminating", "list-error", "absent", "replaced"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()

			kube, pod, manager, _ := replicaFixture(t)
			if err := manager.AcquireLeadership(ctx, "leader"); err != nil {
				t.Fatal(err)
			}

			id := pki.Identity{Kind: pki.Node, PodUID: string(pod.UID), PodName: pod.Name, BootID: "boot", Universe: strings.Repeat("a", 64), Node: strings.Repeat("b", 64)}
			if err := manager.Admit(ctx, id); err != nil {
				t.Fatal(err)
			}

			control := &tlsControl{kube: kube, namespace: pod.Namespace, manager: manager}

			switch mode {
			case "terminating":
				pod.Finalizers = []string{"test/retain"}

				pod.Labels = nil
				if err := kube.Update(ctx, pod); err != nil {
					t.Fatal(err)
				}

				if err := kube.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "list-error":
				control.kube = failedRetirementList{kube}
			case "absent", "replaced":
				if err := kube.Delete(ctx, pod); err != nil {
					t.Fatal(err)
				}

				if mode == "replaced" {
					replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name, UID: "new-uid"}}
					if err := kube.Create(ctx, replacement); err != nil {
						t.Fatal(err)
					}
				}
			}

			if err := control.retireDeletedNodes(ctx); (err != nil) != (mode == "list-error") {
				t.Fatalf("retirement error=%v", err)
			}

			_, err := manager.Member(ctx, id.Key())

			wantRetired := mode == "absent" || mode == "replaced"
			if (err != nil) != wantRetired {
				t.Fatalf("member error=%v want retired=%v", err, wantRetired)
			}
		})
	}
}
