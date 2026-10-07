// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package authority

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestInitializationRequiresStagedProtocol(t *testing.T) {
	for _, state := range []string{"fresh", "consumed"} {
		for _, protocol := range []string{"", "staged-v2"} {
			t.Run(state+"/"+protocol, func(t *testing.T) {
				r := testTopology(t)
				if state == "consumed" {
					require.NoError(t, r.authority.Recover(t.Context(), r.Client))
				}

				reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					err := c.Get(ctx, key, obj, opts...)
					if err == nil && key.Name == r.Config.InstallationConfigMapName {
						obj.(*corev1.ConfigMap).Data[markerInitializationProtocol] = protocol
					}

					return err
				}})
				writer := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						t.Fatal("unsupported protocol created authority")
						return nil
					},
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						t.Fatal("unsupported protocol changed authority")
						return nil
					},
				})
				a := New(r.Config, Dependencies{Reader: reader, Writer: writer})
				require.ErrorIs(t, a.Recover(t.Context(), writer), wire.Unavailable)
				_, _, err := readVersion(t.Context(), reader, r.Config)
				require.ErrorIs(t, err, wire.Unavailable)
			})
		}
	}
}

func TestCommittedCredentialsRequireUIDAndInstallationBinding(t *testing.T) {
	for _, corruption := range []string{"missing uid", "wrong uid", "missing protocol", "wrong installation"} {
		t.Run(corruption, func(t *testing.T) {
			r, _ := testKeyring(t)
			runKeys(t, r)
			reader := interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				err := c.Get(ctx, key, obj, opts...)
				if err == nil && key.Name == r.Config.CredentialsSecretName {
					switch corruption {
					case "missing uid":
						obj.SetUID("")
					case "wrong uid":
						obj.SetUID("replacement")
					case "missing protocol":
						delete(obj.GetAnnotations(), initializationProtocol)
					case "wrong installation":
						obj.GetAnnotations()[installationUIDAnnotation] = "replacement"
					}
				}

				return err
			}})
			issuer := testIssuer(r)
			issuer.APIReader = reader
			_, err := issuer.TrustRoots(t.Context())
			require.ErrorIs(t, err, wire.Unavailable)
			require.False(t, trustReady(r.Trust))
		})
	}
}
