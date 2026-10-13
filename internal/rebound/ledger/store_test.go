// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func storedLedger(t *testing.T) *corev1.ConfigMap {
	t.Helper()

	l := New()
	require.NoError(t, l.Add(testGrant("first", "node-a")))

	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ConfigMapName, Namespace: "system", ResourceVersion: "7"}, Data: l.Encode()}
}

func writeActions(client *fake.Clientset) []clienttesting.Action {
	var writes []clienttesting.Action

	for _, action := range client.Actions() {
		if action.GetVerb() != "get" {
			writes = append(writes, action)
		}
	}

	return writes
}

func TestStoreCreate(t *testing.T) {
	client := fake.NewClientset()
	s := &Store{Client: client, Namespace: "system"}
	l, _, err := s.Update(t.Context(), false, func(l *Ledger) error { return l.Add(testGrant("first", "node-a")) })
	require.NoError(t, err)
	cm, err := client.CoreV1().ConfigMaps("system").Get(t.Context(), ConfigMapName, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, l.Encode(), cm.Data)

	writes := writeActions(client)
	require.Len(t, writes, 1)
	require.Equal(t, "create", writes[0].GetVerb())
}

func TestStoreUpdateAndRetry(t *testing.T) {
	client := fake.NewClientset(storedLedger(t))
	attempts := 0

	client.PrependReactor("update", "configmaps", func(action clienttesting.Action) (bool, runtime.Object, error) {
		update, ok := action.(clienttesting.UpdateAction)
		require.True(t, ok)
		cm, ok := update.GetObject().(*corev1.ConfigMap)
		require.True(t, ok)

		attempts++
		if attempts == 1 {
			require.Equal(t, "7", cm.ResourceVersion)
			fresh := storedLedger(t)
			fresh.ResourceVersion = "8"
			l := Parse(fresh.Data)
			require.NoError(t, l.Add(testGrant("concurrent", "node-c")))
			fresh.Data = l.Encode()
			require.NoError(t, client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), fresh, "system"))

			return true, nil, apierrors.NewConflict(corev1.Resource("configmaps"), ConfigMapName, errors.New("changed"))
		}

		require.Equal(t, "8", cm.ResourceVersion)
		cm.ResourceVersion = "9"

		return false, nil, nil
	})
	s := &Store{Client: client, Namespace: "system"}
	mutations := 0
	l, rv, err := s.Update(t.Context(), false, func(l *Ledger) error {
		mutations++
		return l.Add(testGrant("second", "node-b"))
	})
	require.NoError(t, err)
	require.Equal(t, "9", rv)
	require.Equal(t, 2, attempts)
	require.Equal(t, 2, mutations)
	require.Len(t, l.Grants(), 3)
}

func TestStoreNoWrite(t *testing.T) {
	sentinel := errors.New("mutate failed")
	for _, tt := range []struct {
		name    string
		dryRun  bool
		mutate  func(*Ledger) error
		wantErr error
	}{
		{name: "dry run", dryRun: true, mutate: func(l *Ledger) error { return l.Add(testGrant("second", "node-b")) }},
		{name: "no-op", mutate: func(*Ledger) error { return nil }},
		{name: "mutate error", mutate: func(l *Ledger) error { l.Remove(testGrant("first").Key()); return sentinel }, wantErr: sentinel},
		{name: "too large", mutate: func(l *Ledger) error {
			g := testGrant("second", "node-b")
			g.Holder = strings.Repeat("x", MaxSize)

			return l.Add(g)
		}, wantErr: ErrTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := storedLedger(t)
			client := fake.NewClientset(before.DeepCopy())
			s := &Store{Client: client, Namespace: "system"}
			l, rv, err := s.Update(t.Context(), tt.dryRun, tt.mutate)
			require.ErrorIs(t, err, tt.wantErr)

			if tt.wantErr == nil {
				require.Equal(t, "7", rv)
				require.NotNil(t, l)
			} else {
				require.Same(t, tt.wantErr, err)
			}

			require.Empty(t, writeActions(client))
			after, err := client.CoreV1().ConfigMaps("system").Get(t.Context(), ConfigMapName, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, before.Data, after.Data)
		})
	}
}

func TestStoreMissingNoWrite(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		client := fake.NewClientset()
		s := &Store{Client: client, Namespace: "system"}
		l, rv, err := s.Update(t.Context(), dryRun, func(l *Ledger) error {
			if dryRun {
				return l.Add(testGrant("first", "node-a"))
			}

			return nil
		})
		require.NoError(t, err)
		require.NotNil(t, l)
		require.Empty(t, rv)
		require.Empty(t, writeActions(client))
	}
}
