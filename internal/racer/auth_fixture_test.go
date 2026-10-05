// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"encoding/base64"
	"fmt"
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
	a := assembleFixture(r.Config, r.Client, r.APIReader)
	status := authv1.TokenReviewStatus{Authenticated: true, Audiences: []string{wire.TokenAudience}, User: authv1.UserInfo{Username: "system:serviceaccount:racer:racer-dataplane", UID: string(sa.UID), Extra: map[string]authv1.ExtraValue{"authentication.kubernetes.io/pod-name": {pod.Name}, "authentication.kubernetes.io/pod-uid": {string(pod.UID)}, "authentication.kubernetes.io/node-name": {node.Name}, "authentication.kubernetes.io/node-uid": {string(node.UID)}}}}
	token := "header." + base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, `{"exp":%d}`, time.Now().Add(time.Hour).Unix())) + ".signature"

	return a, status, token
}

func installReview(t *testing.T, a *Application, status authv1.TokenReviewStatus, token string) {
	t.Helper()

	fixtureDependencies[a.authority].Client = interceptor.NewClient(a.Topology.Client.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
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
