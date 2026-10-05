// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	racerv1 "github.com/Azure/unbounded/api/racer/v1alpha1"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestPublicKeyringRotationPinsWriteAdmission(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := authority.Config{Cluster: "11111111-1111-4111-8111-111111111111", Namespace: "racer", DataplaneServiceAccount: "racer-dataplane", DaemonSetName: "racer-dataplane", CredentialsSecretName: "racer-credentials", VersionConfigMapName: "racer-version", InstallationConfigMapName: "racer-installation", SnapshotMaxAge: 5 * time.Second, Rotation: authority.RotationPolicy{Interval: 24 * time.Hour, PrepareFor: time.Hour, RetainFor: 48 * time.Hour}}

		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}

		if err := racerv1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}

		marker := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: cfg.Namespace, Name: cfg.InstallationConfigMapName, UID: "installation"}, Data: map[string]string{"cluster": string(cfg.Cluster), "version_configmap": cfg.VersionConfigMapName, "state": "fresh"}}
		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(marker).Build()
		now := time.Now()

		a := authority.New(cfg, authority.Dependencies{Reader: c, Writer: c, Now: func() time.Time { return now }})
		if err := a.Recover(t.Context(), c); err != nil {
			t.Fatal(err)
		}

		if _, err := a.ReconcileCredentials(t.Context()); err != nil {
			t.Fatal(err)
		}

		old, err := a.Keyring()
		if err != nil {
			t.Fatal(err)
		}

		admitted, stop, err := a.TrustContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer stop()

		deadline, _ := admitted.Deadline()

		var before bytes.Buffer
		if _, err := old.Response().WriteTo(admitted, &before); err != nil {
			t.Fatal(err)
		}

		time.Sleep(3 * time.Second)

		now = now.Add(cfg.Rotation.Interval - cfg.Rotation.PrepareFor)

		if _, err := a.ReconcileCredentials(t.Context()); err != nil {
			t.Fatal(err)
		}

		current, err := a.Keyring()
		if err != nil {
			t.Fatal(err)
		}

		if current.Generation() <= old.Generation() {
			t.Fatal("ordinary rotation did not advance bundle")
		}

		fresh, stopFresh, err := a.TrustContext(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer stopFresh()

		if _, err := old.Response().WriteTo(fresh, io.Discard); !errors.Is(err, wire.Forbidden) {
			t.Fatal("superseded handle borrowed fresh admission", err)
		}

		if _, err := current.Response().WriteTo(fresh, io.Discard); err != nil {
			t.Fatal(err)
		}

		var after bytes.Buffer
		if _, err := old.Response().WriteTo(admitted, &after); err != nil {
			t.Fatal("rotation revoked admitted write", err)
		}

		if !bytes.Equal(before.Bytes(), after.Bytes()) {
			t.Fatal("admitted encoding changed")
		}

		if got, _ := admitted.Deadline(); got != deadline {
			t.Fatal("rotation extended admitted deadline")
		}

		time.Sleep(2 * time.Second)

		if _, err := old.Response().WriteTo(admitted, io.Discard); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("old write outlived pinned freshness", err)
		}
	})
}

func TestPublicAuthorityScaffoldAndOpaqueValues(t *testing.T) {
	a := authority.New(authority.Config{}, authority.Dependencies{})
	if !errors.Is(a.TrustReady(), wire.Unavailable) || !errors.Is(a.PublicationReady(), wire.Unavailable) {
		t.Fatal("constructor granted authority")
	}

	if !errors.Is(a.Recover(t.Context(), nil), wire.InvalidRequest) {
		t.Fatal("zero configuration reached I/O")
	}

	if _, err := a.Issue(t.Context(), authority.NodeIdentity{}, wire.BootstrapRequest{}); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero identity accepted", err)
	}

	if _, err := a.Wait(t.Context(), authority.NodeIdentity{}, nil); !errors.Is(err, wire.Unauthenticated) {
		t.Fatal("zero poll identity accepted", err)
	}

	var handle authority.PublicationHandle
	if _, _, err := handle.WriteContext(t.Context()); !errors.Is(err, wire.Unavailable) {
		t.Fatal("zero publication handle accepted", err)
	}

	if _, err := handle.ForBase("").WriteTo(context.Background(), io.Discard); !errors.Is(err, wire.Forbidden) {
		t.Fatal("unguarded response accepted", err)
	}

	for _, value := range []any{authority.NodeIdentity{}, authority.ReplicaIdentity{}, authority.PublicationHandle{}, authority.KeyringHandle{}, authority.Response{}, *a} {
		typeOf := reflect.TypeOf(value)
		for i := range typeOf.NumField() {
			if typeOf.Field(i).IsExported() {
				t.Fatalf("%s exposes mutable field %s", typeOf.Name(), typeOf.Field(i).Name)
			}
		}
	}
}
