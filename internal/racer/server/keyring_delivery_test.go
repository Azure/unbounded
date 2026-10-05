// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestKeyringReauthenticationCannotDiscloseWithdrawnBundle(t *testing.T) {
	f := newServingFixture(t)
	reads := 0
	fixtureDependencies[f.a.authority].reader = interceptor.NewClient(f.a.Topology.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
		if _, ok := obj.(*corev1.Pod); ok {
			reads++
			if reads == 2 {
				invalidateFixtureTrust(t, f)
			}
		}

		return c.Get(ctx, key, obj, opts...)
	}})
	w := httptest.NewRecorder()
	f.a.Server.Handler().ServeHTTP(w, keyringRequest(t, f, true, ""))
	requireKeyringResponse(t, w, 503)

	if reads != 2 {
		t.Fatal("bearer not rechecked before response")
	}
}
