// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	machina "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/racer/pki"
)

func restartedNodeFixture(t *testing.T, options pki.Options) (client.WithWatch, *corev1.Pod, *pki.Manager) {
	t.Helper()

	pod, daemon, node, site := enrollmentObjects()
	pod.Finalizers = []string{"test/retain"}
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "dataplane", ContainerID: "containerd://preceding", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, appsv1.AddToScheme, machina.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod, daemon, node, site).Build()

	manager, err := pki.New(kube, pod.Namespace, options)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.AcquireLeadership(t.Context(), "leader"); err != nil {
		t.Fatal(err)
	}

	if err := manager.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}

	return kube, pod, manager
}

func TestSamePodRestartRecoversCARotation(t *testing.T) {
	ctx := t.Context()
	kube, pod, manager := restartedNodeFixture(t, pki.Options{LeafLifetime: 3 * time.Second, ClockSkew: time.Millisecond})
	control := &tlsControl{kube: kube, namespace: pod.Namespace, manager: manager}
	server := &enrollmentServer{
		kube: kube, review: &reviewTestClient{review: validReview}, namespace: pod.Namespace,
		selected: func(id enrollmentIdentity) bool { return id.podUID == string(pod.UID) },
		renewal: func(ctx context.Context, key types.NamespacedName, uid, boot string) (enrollmentIdentity, error) {
			return retainedEnrollmentIdentity(ctx, kube, manager, key, uid, boot)
		},
		issue: func(_ context.Context, _ string, id enrollmentIdentity) (enrollmentResponse, error) {
			issued, _ := issueTLSFixture(t, manager, pki.Identity{Kind: pki.Node, Universe: id.universe, Node: id.node, PodUID: id.podUID, BootID: id.boot, ContainerID: id.containerID, PodName: id.podName}, false)
			return enrollmentResponse{Certificate: string(issued.CertificatePEM)}, nil
		},
	}
	enroll := func(boot, token string, want int) {
		t.Helper()

		req := httptest.NewRequest(http.MethodPost, "https://control/v3/enroll", strings.NewReader(`{"csr":"fixture","pod_namespace":"system","pod_name":"racer-worker"}`))
		req.Header.Set("X-Racer-Boot", boot)

		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		w := httptest.NewRecorder()
		server.enroll(w, req)

		if w.Code != want {
			t.Fatalf("enrollment status=%d want=%d body=%s", w.Code, want, w.Body.String())
		}
	}
	oldBoot, newBoot := strings.Repeat("a", 64), strings.Repeat("b", 64)
	enroll(oldBoot, "token", http.StatusOK)
	enroll(newBoot, "", http.StatusForbidden)

	if err := control.retireDeletedNodes(ctx); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp != nil {
		t.Fatalf("unauthenticated boot triggered replacement: %v", err)
	}

	// The restarted process authenticates before kubelet publishes new status.
	enroll(newBoot, "token", http.StatusOK)

	members, err := manager.Members(ctx)
	if err != nil || len(members) != 2 || members[0].ContainerID != members[1].ContainerID {
		t.Fatalf("expected two boots with ambiguous container status: %+v %v", members, err)
	}

	pod.Status.ContainerStatuses[0].ContainerID = "containerd://current"

	pod.Status.ContainerStatuses[0].LastTerminationState.Terminated = &corev1.ContainerStateTerminated{ContainerID: "containerd://preceding"}
	if err := kube.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if err := manager.TriggerRotation(ctx); err != nil {
		t.Fatal(err)
	}

	if err := control.retireDeletedNodes(ctx); err != nil {
		t.Fatal(err)
	}

	if err := kube.Get(ctx, client.ObjectKeyFromObject(pod), pod); err != nil || pod.DeletionTimestamp == nil {
		t.Fatalf("restart did not request graceful replacement: %v", err)
	}
	// Even a fresh proof from the new boot cannot stand in for its predecessor.
	cp := pki.Identity{Kind: pki.ControlPlane, PodUID: "controller", PodName: "controller", BootID: "leader"}
	if err := kube.Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: cp.PodName, UID: types.UID(cp.PodUID)}}); err != nil {
		t.Fatal(err)
	}

	proveRestartRotation(t, manager, members[1], cp)

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertRotationGeneration(t, manager, 2)

	if err := control.retireDeletedNodes(ctx); err != nil {
		t.Fatal(err)
	}

	for _, id := range members {
		if _, err := manager.Member(ctx, id.Key()); err != nil {
			t.Fatalf("terminating Pod lost boot %s: %v", id.BootID, err)
		}
	}

	// Simulate kubelet completing graceful termination, then DaemonSet replacement.
	pod.Finalizers = nil
	if err := kube.Update(ctx, pod); err != nil {
		t.Fatal(err)
	}

	replacement, _, _, _ := enrollmentObjects()

	replacement.UID = "replacement-uid"
	if err := kube.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	if err := control.retireDeletedNodes(ctx); err != nil {
		t.Fatal(err)
	}

	for _, id := range members {
		if _, err := manager.Member(ctx, id.Key()); err == nil {
			t.Fatal("absent Pod boot remains in barrier")
		}

		if err := manager.Admit(ctx, id); err == nil {
			t.Fatal("retired boot re-admitted")
		}
	}
	// A cached credential for the deleted UID cannot enroll against its replacement.
	enroll(strings.Repeat("c", 64), "token", http.StatusForbidden)

	if err := manager.CollectRetirements(ctx); err != nil {
		t.Fatal(err)
	}

	if err := manager.Admit(ctx, members[0]); err == nil {
		t.Fatal("deleted UID re-admitted after tombstone collection")
	}

	id := members[1]
	id.PodUID, id.BootID, id.ContainerID = string(replacement.UID), "replacement-boot", ""
	proveRestartRotation(t, manager, id, cp)

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertRotationGeneration(t, manager, 3)
	proveRestartRotation(t, manager, id, cp)

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertRotationGeneration(t, manager, 3)
	// Wait for all old-root leaves, including retired boots, to expire plus skew.
	time.Sleep(3100 * time.Millisecond)
	proveRestartRotation(t, manager, id, cp)

	if err := manager.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}

	assertRotationGeneration(t, manager, 4)

	bundle, err := manager.Bundle(ctx)
	if err != nil || strings.Count(bundle.Certificates, "BEGIN CERTIFICATE") != 1 {
		t.Fatalf("old CA was not removed: %v", err)
	}
}

func assertRotationGeneration(t *testing.T, manager *pki.Manager, want uint64) {
	t.Helper()

	bundle, err := manager.Bundle(t.Context())
	if err != nil || bundle.Generation != want {
		t.Fatalf("rotation generation=%d want=%d error=%v", bundle.Generation, want, err)
	}
}

// Both rotation participants produce fresh cryptographic proofs, with the node's
// acknowledgment carried on the authenticated session, rather than test-injected
// proof fields. The CP uses the pending-root proof certificate during overlap.
func proveRestartRotation(t *testing.T, manager *pki.Manager, node, cp pki.Identity) {
	t.Helper()

	nodeLeaf, nodeKey := issueTLSFixture(t, manager, node, false)
	cpLeaf, cpKey := issueTLSFixture(t, manager, cp, true)

	nodeHot, cpHot := pki.NewHotTLS(), pki.NewHotTLS()
	if err := nodeHot.Update(nodeLeaf.Bundle.JSON(), nodeLeaf.CertificatePEM, nodeKey); err != nil {
		t.Fatal(err)
	}

	if err := cpHot.Update(cpLeaf.Bundle.JSON(), cpLeaf.CertificatePEM, cpKey); err != nil {
		t.Fatal(err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		raw, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer raw.Close()

		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))

		conn, finish, err := cpHot.HandshakeProof(ctx, raw, true, "")
		if err != nil {
			done <- err
			return
		}

		var ack pki.Acknowledgment
		if err := json.NewDecoder(conn).Decode(&ack); err != nil {
			done <- err
			return
		}

		proof, err := finish(ack)
		if err == nil {
			err = manager.RecordTLSProof(ctx, node.Key(), proof)
		}

		done <- err
	}()

	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()

	conn, finish, err := nodeHot.HandshakeProof(ctx, raw, false, "racer-controlplane.system.svc")
	if err != nil {
		t.Fatal(err)
	}

	ack := pki.Acknowledgment{Generation: cpLeaf.Bundle.Generation, Digest: cpLeaf.Bundle.Digest(), OldConnectionsDrained: true}
	if err := json.NewEncoder(conn).Encode(ack); err != nil {
		t.Fatal(err)
	}

	proof, err := finish(ack)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.RecordTLSProof(ctx, cp.Key(), proof); err != nil {
		t.Fatal(err)
	}

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRestartReplacementRequiresManagedPodAndSafeDelete(t *testing.T) {
	for _, mode := range []string{"concurrent-boots", "single-boot", "controlplane", "wrong-account", "no-owner", "wrong-owner-uid", "unmanaged-daemon", "list-error", "delete-error", "replacement-race"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			kube, pod, manager := restartedNodeFixture(t, pki.Options{})

			for _, boot := range []string{"a", "b"} {
				if mode == "single-boot" && boot == "b" {
					continue
				}

				id := pki.Identity{Kind: pki.Node, Universe: strings.Repeat("a", 64), Node: strings.Repeat("b", 64), PodUID: string(pod.UID), PodName: pod.Name, BootID: boot}
				if mode == "controlplane" {
					id.Kind, id.Universe, id.Node = pki.ControlPlane, "", ""
				}

				if err := manager.Admit(ctx, id); err != nil {
					t.Fatal(err)
				}
			}

			switch mode {
			case "wrong-account":
				pod.Spec.ServiceAccountName = "other"
			case "no-owner":
				pod.OwnerReferences = nil
			case "wrong-owner-uid":
				pod.OwnerReferences[0].UID = "other"
			case "unmanaged-daemon":
				var daemon appsv1.DaemonSet
				if err := kube.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: pod.OwnerReferences[0].Name}, &daemon); err != nil {
					t.Fatal(err)
				}

				daemon.Labels = nil
				if err := kube.Update(ctx, &daemon); err != nil {
					t.Fatal(err)
				}
			}

			if err := kube.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}

			deletes := 0
			guarded := interceptor.NewClient(kube, interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				deletes++

				options := (&client.DeleteOptions{}).ApplyOptions(opts)
				if options.GracePeriodSeconds != nil || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != pod.UID || options.Preconditions.ResourceVersion == nil || *options.Preconditions.ResourceVersion != pod.ResourceVersion {
					t.Fatalf("unsafe Pod deletion: %+v", options)
				}

				if mode == "delete-error" {
					return errors.New("delete forbidden")
				}

				if mode == "replacement-race" {
					// API preconditions reject a name now bound to another UID.
					return apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, errors.New("UID precondition failed"))
				}

				return c.Delete(ctx, obj, opts...)
			}})

			control := &tlsControl{kube: guarded, namespace: pod.Namespace, manager: manager}
			if mode == "list-error" {
				control.kube = failedRetirementList{guarded}
			}

			err := control.retireDeletedNodes(ctx)

			wantError := mode == "list-error" || mode == "delete-error" || mode == "replacement-race"
			if (err != nil) != wantError {
				t.Fatalf("replacement error=%v", err)
			}

			wantDelete := mode == "concurrent-boots" || mode == "delete-error" || mode == "replacement-race"
			if (deletes == 1) != wantDelete {
				t.Fatalf("delete calls=%d want delete=%v", deletes, wantDelete)
			}

			members, err := manager.Members(ctx)

			wantMembers := 2
			if mode == "single-boot" {
				wantMembers = 1
			}

			if err != nil || len(members) != wantMembers {
				t.Fatalf("replacement prematurely retired members: %+v %v", members, err)
			}
		})
	}
}
