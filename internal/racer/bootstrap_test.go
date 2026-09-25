// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func authFixture(t *testing.T) (*Application, authv1.TokenReviewStatus, string) {
	t.Helper()

	controller := true
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "ds-uid"}}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "racer-dataplane", UID: "sa-uid"}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID(testNodeUID)}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "racer", Name: "worker-pod", UID: "pod-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID, Controller: &controller}}}, Spec: corev1.PodSpec{NodeName: node.Name, ServiceAccountName: sa.Name}, Status: corev1.PodStatus{PodIP: "192.0.2.1"}}
	r := initializedTopology(t, ds, sa, node, pod)
	a := Assemble(r.Config, r.Client, r.APIReader)
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:racer:racer-dataplane", UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}}}}
	token := "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".signature"

	return a, status, token
}

func installReview(t *testing.T, a *Application, status authv1.TokenReviewStatus, token string) {
	t.Helper()

	a.Server.Bootstrap.Client = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		review, ok := obj.(*authv1.TokenReview)
		if !ok {
			return c.Create(ctx, obj, opts...)
		}

		if review.Spec.Token != token || len(review.Spec.Audiences) != 1 || review.Spec.Audiences[0] != wire.TokenAudience {
			t.Error("TokenReview did not bind token/audience")
		}

		review.Status = *status.DeepCopy()

		return ctx.Err()
	}})
}

func TestBootstrapAuthoritativeBindings(t *testing.T) {
	for _, scenario := range []string{"success", "audience", "not authenticated", "review error", "username", "sa uid", "pod uid", "missing bound pod", "ambiguous bound pod", "node extra", "node extra uid", "recreated pod", "recreated sa", "recreated ds", "owner name", "owner kind", "owner not controller", "pod sa", "unscheduled", "terminal pod", "excluded node", "deleted node", "api failure", "canceled", "expired token", "duplicate bearer"} {
		t.Run(scenario, func(t *testing.T) {
			a, status, token := authFixture(t)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			pod := &corev1.Pod{}
			if err := a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "worker-pod"}, pod); err != nil {
				t.Fatal(err)
			}

			switch scenario {
			case "audience":
				status.Audiences = []string{"api"}
			case "not authenticated":
				status.Authenticated = false
			case "review error":
				status.Error = "private upstream details"
			case "username":
				status.User.Username = "system:serviceaccount:other:racer-dataplane"
			case "sa uid":
				status.User.UID = "old-sa"
			case "pod uid":
				status.User.Extra["authentication.kubernetes.io/pod-uid"] = authv1.ExtraValue{"old-pod"}
			case "missing bound pod":
				delete(status.User.Extra, "authentication.kubernetes.io/pod-name")
			case "ambiguous bound pod":
				status.User.Extra["authentication.kubernetes.io/pod-name"] = authv1.ExtraValue{"worker-pod", "other"}
			case "node extra":
				status.User.Extra["authentication.kubernetes.io/node-name"] = authv1.ExtraValue{"other"}
			case "node extra uid":
				status.User.Extra["authentication.kubernetes.io/node-uid"] = authv1.ExtraValue{"old"}
			case "recreated pod":
				pod.UID = "replacement"
			case "recreated sa", "recreated ds":
				var obj client.Object = &corev1.ServiceAccount{}
				if scenario == "recreated ds" {
					obj = &appsv1.DaemonSet{}
				}

				if err := a.Topology.Get(ctx, client.ObjectKey{Namespace: "racer", Name: "racer-dataplane"}, obj); err != nil {
					t.Fatal(err)
				}

				obj.SetUID("replacement")

				if err := a.Topology.Update(ctx, obj); err != nil {
					t.Fatal(err)
				}
			case "owner name":
				pod.OwnerReferences[0].Name = "other"
			case "owner kind":
				pod.OwnerReferences[0].Kind = "ReplicaSet"
			case "owner not controller":
				pod.OwnerReferences[0].Controller = nil
			case "pod sa":
				pod.Spec.ServiceAccountName = "other"
			case "unscheduled":
				pod.Spec.NodeName = ""
			case "terminal pod":
				pod.Status.Phase = corev1.PodFailed
				if err := a.Topology.Client.Status().Update(ctx, pod); err != nil {
					t.Fatal(err)
				}
			case "excluded node", "deleted node":
				node := &corev1.Node{}
				if err := a.Topology.Get(ctx, client.ObjectKey{Name: "worker"}, node); err != nil {
					t.Fatal(err)
				}

				if scenario == "deleted node" {
					if err := a.Topology.Delete(ctx, node); err != nil {
						t.Fatal(err)
					}
				} else {
					node.Labels = map[string]string{wire.ExclusionLabel: ""}
					if err := a.Topology.Update(ctx, node); err != nil {
						t.Fatal(err)
					}
				}
			case "expired token":
				token = "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1}`)) + ".signature"
			}

			if err := a.Topology.Update(ctx, pod); err != nil {
				t.Fatal(err)
			}

			installReview(t, a, status, token)

			if scenario == "api failure" {
				a.Server.Bootstrap.APIReader = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					return fmt.Errorf("private upstream failure")
				}})
			}

			if scenario == "canceled" {
				cancel()
			}

			req := httptest.NewRequest("POST", wire.BootstrapPath, nil)
			req.Header.Set("Authorization", "Bearer "+token)

			if scenario == "duplicate bearer" {
				req.Header.Add("Authorization", "Bearer "+token)
			}

			identity, err := a.Server.Bootstrap.Authenticate(ctx, req)
			if scenario == "success" {
				if err != nil || identity.node != wire.NodeID(testNodeUID) || identity.cluster != a.Server.Config.Cluster || !identity.expires.After(time.Now()) {
					t.Fatalf("identity: %+v %v", identity, err)
				}
			} else if err == nil || identity.node != "" {
				t.Fatalf("unauthorized identity: %+v %v", identity, err)
			}
		})
	}
}
