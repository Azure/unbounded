// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestCredentialsCacheSelector(t *testing.T) {
	for _, name := range []string{"racer-credentials", "custom-credentials"} {
		t.Run(name, func(t *testing.T) {
			options := managerOptions(Config{Namespace: "custom-system", CredentialsSecretName: name}, runtime.NewScheme())
			for obj, config := range options.Cache.ByObject {
				if _, ok := obj.(*corev1.Secret); !ok {
					continue
				}

				require.Len(t, config.Namespaces, 1)
				require.Contains(t, config.Namespaces, "custom-system")
				require.NotNil(t, config.Field)
				require.Equal(t, "metadata.name="+name, config.Field.String())
				require.True(t, config.Field.Matches(fields.Set{"metadata.name": name}))

				for _, excluded := range []string{"racer-controller-tls", "unrelated", ""} {
					require.False(t, config.Field.Matches(fields.Set{"metadata.name": excluded}))
				}

				return
			}

			t.Fatal("Secret cache configuration missing")
		})
	}
}
