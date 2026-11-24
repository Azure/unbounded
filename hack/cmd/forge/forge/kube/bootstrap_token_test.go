// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGetBootstrapToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		secret  string
		tokenID string
		want    *BootstrapToken
	}{
		{name: "name matches token-id", secret: "bootstrap-token-abc123", tokenID: "abc123", want: &BootstrapToken{ID: "abc123", Secret: "0123456789abcdef"}},
		{name: "missing token-id", secret: "bootstrap-token-abc123"},
		{name: "name does not match token-id", secret: "bootstrap-token-abc123", tokenID: "def456"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			data := map[string][]byte{"token-secret": []byte("0123456789abcdef")}
			if tc.tokenID != "" {
				data["token-id"] = []byte(tc.tokenID)
			}

			client := fake.NewClientset(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tc.secret,
					Namespace: metav1.NamespaceSystem,
					Labels:    map[string]string{"unbounded-cloud.io/default-bootstrap-token": "true"},
				},
				Type: corev1.SecretTypeBootstrapToken,
				Data: data,
			})

			got, err := GetBootstrapToken(t.Context(), client)
			if tc.want == nil {
				require.ErrorIs(t, err, ErrBootstrapTokenNotFound)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}
