// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/operator/component"
)

func tlsEpoch() time.Time {
	return time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
}

func verifyServing(t *testing.T, secret *corev1.Secret, trust []byte, now time.Time) error {
	t.Helper()

	pair, err := tls.X509KeyPair(secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey])
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)

	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(trust))

	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		require.NoError(t, err)
		intermediates.AddCert(cert)
	}

	_, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, CurrentTime: now, DNSName: controllerName + ".custom-system.svc"})

	return err
}

func stateOf(t *testing.T, secret *corev1.Secret) tlsState {
	t.Helper()

	var state tlsState
	require.NoError(t, json.Unmarshal(secret.Data[tlsStateKey], &state))

	return state
}

func TestTLSReplicationProjectionOrders(t *testing.T) {
	now := tlsEpoch()
	old, err := newTLS("custom-system", now)
	require.NoError(t, err)

	at := now.Add(caRotationInterval)
	rotated, err := renewTLS(old, "custom-system", at)
	require.NoError(t, err)
	unrelated, err := newTLS("custom-system", at)
	require.NoError(t, err)

	// Reproduce the failure with current-only trust before checking the bundle.
	require.Error(t, verifyServing(t, old, rotated.Data["ca.crt"], at))

	for _, scenario := range []struct {
		name     string
		leader   *corev1.Secret
		follower *corev1.Secret
	}{
		{"neither-projected", old, old},
		{"follower-first", old, rotated},
		{"leader-first", rotated, old},
		{"both-projected", rotated, rotated},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			require.NoError(t, verifyServing(t, scenario.leader, scenario.follower.Data[caBundleKey], at))
			require.Error(t, verifyServing(t, unrelated, scenario.follower.Data[caBundleKey], at))
		})
	}
}

func TestTLSReplicationBundleRepair(t *testing.T) {
	now := tlsEpoch()
	fresh, err := newTLS("custom-system", now)
	require.NoError(t, err)
	rotated, err := renewTLS(fresh, "custom-system", now.Add(caRotationInterval))
	require.NoError(t, err)
	unrelated, err := newTLS("custom-system", now)
	require.NoError(t, err)

	for _, original := range []*corev1.Secret{fresh, rotated} {
		for _, scenario := range []string{"missing", "empty", "stale", "foreign"} {
			t.Run(stateOf(t, original).CreatedAt.Format("2006-01-02")+"/"+scenario, func(t *testing.T) {
				secret := original.DeepCopy()

				switch scenario {
				case "missing":
					delete(secret.Data, caBundleKey)
				case "empty":
					secret.Data[caBundleKey] = []byte{}
				case "stale":
					secret.Data[caBundleKey] = []byte("invalid PEM")
				case "foreign":
					secret.Data[caBundleKey] = unrelated.Data["ca.crt"]
				}

				secret.Annotations["unrelated"] = "preserved"
				secret.Data["unrelated"] = []byte("preserved")
				env := testEnv(t, secret)
				at := stateOf(t, original).CreatedAt.Add(time.Hour)
				plan := component.NewPlan()
				result, err := planTLSAt(t.Context(), env, plan, false, at)
				require.NoError(t, err)
				require.Nil(t, result, "do not publish workloads before bundle persistence")
				require.Len(t, plan.Operations, 1)
				require.Equal(t, component.OpMergePatch, plan.Operations[0].Kind)

				stored := &corev1.Secret{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
				require.Equal(t, secret.Data, stored.Data, "planning must not mutate credentials")
				persist(t, env, plan)

				restart := component.NewPlan()
				result, err = planTLSAt(t.Context(), env, restart, false, at)
				require.NoError(t, err)
				require.Zero(t, restart.Len())

				want := secret.DeepCopy()
				want.Data[caBundleKey] = original.Data[caBundleKey]
				require.Equal(t, want.Data, result.Data, "only the derived bundle changes")
				require.Equal(t, want.Annotations, result.Annotations)
				require.Equal(t, string(original.Data["ca.crt"])+string(original.Data[previousCAKey]), string(result.Data[caBundleKey]))
				require.NoError(t, verifyServing(t, fresh, result.Data[caBundleKey], at))
				require.Error(t, verifyServing(t, unrelated, result.Data[caBundleKey], at))
			})
		}
	}
}

func TestTLSReplicationBundleUpgradeBeforeWorkloads(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "active", true: "retained"}[retained], func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)
			persist(t, env, planPass(t, env))

			if retained {
				require.NoError(t, env.Client.Delete(t.Context(), cache("cache")))
			}

			secret := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), secret))
			delete(secret.Data, caBundleKey)
			require.NoError(t, env.Client.Update(t.Context(), secret))
			at := stateOf(t, secret).CreatedAt.Add(time.Hour)
			plan, _, err := planAt(t.Context(), env, at)
			require.NoError(t, err)
			require.Len(t, plan.Operations, 1, "persist bundle before updating projected workload")
			require.Equal(t, component.OpMergePatch, plan.Operations[0].Kind)
			require.Equal(t, tlsName, plan.Operations[0].Object.GetName())
			persist(t, env, plan)

			stored := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
			require.Equal(t, secret.Data["ca.crt"], stored.Data[caBundleKey])
			require.Equal(t, secret.Data[corev1.TLSPrivateKeyKey], stored.Data[corev1.TLSPrivateKeyKey])
			plan, _, err = planAt(t.Context(), env, at)
			require.NoError(t, err)

			if retained {
				require.Zero(t, plan.Len(), "retained mode must not repair workloads")
			} else {
				require.NotZero(t, plan.Len(), "active mode resumes runtime reconciliation")
			}
		})
	}
}

func TestTLSWeeklyGenerations(t *testing.T) {
	now := tlsEpoch()
	secret, err := newTLS("custom-system", now)
	require.NoError(t, err)
	ca, err := singleCertificate(secret.Data["ca.crt"])
	require.NoError(t, err)
	require.Equal(t, now.Add(caLifetime), ca.NotAfter)

	leaf, err := singleCertificate(secret.Data[corev1.TLSCertKey])
	require.NoError(t, err)
	require.Equal(t, now.Add(leafLifetime), leaf.NotAfter)

	roots := [][]byte{secret.Data["ca.crt"]}
	keys := [][]byte{secret.Data["ca.key"]}

	for generation := 1; generation <= 8; generation++ {
		at := now.Add(time.Duration(generation) * caRotationInterval)
		unchanged, err := renewTLS(secret, "custom-system", at.Add(-time.Second))
		require.NoError(t, err)
		require.Nil(t, unchanged)

		next, err := renewTLS(secret, "custom-system", at)
		require.NoError(t, err)
		require.NotNil(t, next)
		require.NotEqual(t, secret.Data[corev1.TLSPrivateKeyKey], next.Data[corev1.TLSPrivateKeyKey])

		for _, oldKey := range keys {
			require.NotEqual(t, oldKey, next.Data["ca.key"])
		}

		keys = append(keys, next.Data["ca.key"])
		roots = append(roots, next.Data["ca.crt"])
		require.Len(t, stateOf(t, next).Previous, min(generation, maxPreviousCAs))
		require.NoError(t, verifyServing(t, next, next.Data["ca.crt"], at))
		require.Equal(t, string(next.Data["ca.crt"])+string(next.Data[previousCAKey]), string(next.Data[caBundleKey]))

		bundle := x509.NewCertPool()
		require.True(t, bundle.AppendCertsFromPEM(next.Data[caBundleKey]))

		for i, root := range roots {
			ca, err := singleCertificate(root)
			require.NoError(t, err)

			_, err = ca.Verify(x509.VerifyOptions{Roots: bundle, CurrentTime: at})
			if i >= generation-maxPreviousCAs {
				require.NoError(t, err, "retained root %d", i)
			} else {
				require.NotContains(t, string(next.Data[caBundleKey]), string(root))
				require.Error(t, err, "retired root %d", i)
			}
		}

		for i, root := range roots[:generation] {
			err := verifyServing(t, next, root, at)
			if i >= generation-maxPreviousCAs {
				require.NoError(t, err, "generation %d through root %d", generation, i)
			} else {
				require.Error(t, err, "retired root %d", i)
			}
		}
		// Every public compatibility CA can also serve as the trust anchor.
		for _, previous := range stateOf(t, next).Previous {
			require.NoError(t, verifyServing(t, next, previous.Cross, at))
		}
		// Reload all state from the serialized Secret, not process memory.
		env := testEnv(t, next)
		plan := component.NewPlan()
		persisted, err := planTLSAt(t.Context(), env, plan, false, at)
		require.NoError(t, err)
		require.Equal(t, next.Data, persisted.Data)
		require.Zero(t, plan.Len())

		secret = next
	}
}

func TestTLSLeafRenewalAndDelayedRotation(t *testing.T) {
	now := tlsEpoch()
	secret, err := newTLS("custom-system", now)
	require.NoError(t, err)
	caPair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
	require.NoError(t, err)
	ca, err := singleCertificate(secret.Data["ca.crt"])
	require.NoError(t, err)
	// A short-lived but valid leaf is renewed independently of the CA clock.
	secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], err = issueLeaf("custom-system", ca, caPair.PrivateKey.(*ecdsa.PrivateKey), now.Add(-8*day))
	require.NoError(t, err)
	next, err := renewTLS(secret, "custom-system", now)
	require.NoError(t, err)
	require.NotNil(t, next)
	require.Equal(t, secret.Data["ca.key"], next.Data["ca.key"])
	require.NoError(t, verifyServing(t, next, secret.Data["ca.crt"], now))
	// A long outage permits recovery while the CA is valid, even if the
	// leaf expired. The compatibility path ends at the old CA's expiry.
	next, err = renewTLS(next, "custom-system", now.Add(27*day))
	require.NoError(t, err)
	require.NoError(t, verifyServing(t, next, secret.Data["ca.crt"], now.Add(27*day)))
	require.Equal(t, now.Add(28*day), stateOf(t, next).Previous[0].RetireAt)
	pruned, err := renewTLS(next, "custom-system", now.Add(28*day))
	require.NoError(t, err)
	require.Empty(t, stateOf(t, pruned).Previous)
	require.Empty(t, pruned.Data[previousCAKey])
	require.Equal(t, pruned.Data["ca.crt"], pruned.Data[caBundleKey])
	require.NotContains(t, string(pruned.Data[caBundleKey]), string(secret.Data["ca.crt"]))
	require.Equal(t, next.Data[corev1.TLSPrivateKeyKey], pruned.Data[corev1.TLSPrivateKeyKey])
	require.NoError(t, verifyServing(t, pruned, pruned.Data["ca.crt"], now.Add(28*day)))
	// Persist the empty previous-root value through real merge-patch encoding.
	env := testEnv(t, next)
	plan := component.NewPlan()
	result, err := planTLSAt(t.Context(), env, plan, false, now.Add(28*day))
	require.NoError(t, err)
	require.Nil(t, result)
	persist(t, env, plan)
	result, err = planTLSAt(t.Context(), env, component.NewPlan(), false, now.Add(28*day))
	require.NoError(t, err)
	require.Equal(t, pruned.Data, result.Data)

	_, err = renewTLS(secret, "custom-system", now.Add(28*day))
	require.ErrorContains(t, err, "CA expired")
}

func TestTLSCreateRacesAndReadFailures(t *testing.T) {
	now := tlsEpoch()

	for _, scenario := range []string{"winner", "lost-response", "read-error", "trust-read-error", "established-missing", "retained-trust"} {
		t.Run(scenario, func(t *testing.T) {
			env := testEnv(t)
			if scenario == "retained-trust" {
				require.NoError(t, env.Client.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: trustName, Namespace: env.Namespace}}))
			}

			if scenario == "read-error" || scenario == "trust-read-error" {
				env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if scenario == "read-error" || key.Name == trustName {
							return errors.New("read unavailable")
						}

						return c.Get(ctx, key, obj, opts...)
					},
				})
			}

			plan := component.NewPlan()
			result, err := planTLSAt(t.Context(), env, plan, scenario != "established-missing", now)
			require.Nil(t, result)

			if scenario != "winner" && scenario != "lost-response" {
				require.Error(t, err)
				require.Zero(t, plan.Len())

				return
			}

			require.NoError(t, err)
			require.Len(t, plan.Operations, 1)
			require.Equal(t, component.OpCreateIfAbsent, plan.Operations[0].Kind)

			if scenario == "winner" {
				winner, err := newTLS(env.Namespace, now)
				require.NoError(t, err)
				require.NoError(t, env.Client.Create(t.Context(), winner))
				persist(t, env, plan)
				result, err = planTLSAt(t.Context(), env, component.NewPlan(), false, now)
				require.NoError(t, err)
				require.Equal(t, winner.Data, result.Data)
			} else {
				executor := *env
				executor.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						require.NoError(t, c.Create(ctx, obj, opts...))
						return errors.New("lost create response")
					},
				})
				executed, err := executor.Execute(t.Context(), plan)
				require.NoError(t, err)
				require.Error(t, executed.Err())

				restart := component.NewPlan()
				result, err = planTLSAt(t.Context(), env, restart, false, now)
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Zero(t, restart.Len())
			}
		})
	}
}

func TestTLSLegacyMigration(t *testing.T) {
	now := tlsEpoch()
	secret, err := newTLS("custom-system", now)
	require.NoError(t, err)
	// Re-create the old ten-year CA and one-year leaf format with the old
	// common subject. No rotation metadata existed in those installations.
	pair, err := tls.X509KeyPair(secret.Data["ca.crt"], secret.Data["ca.key"])
	require.NoError(t, err)

	key := pair.PrivateKey.(*ecdsa.PrivateKey)
	ca, err := singleCertificate(secret.Data["ca.crt"])
	require.NoError(t, err)

	ca.Subject.CommonName = "racer-serving-ca"
	ca.RawSubject = nil
	ca.NotAfter = now.Add(10 * 365 * day)
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	require.NoError(t, err)

	secret.Data["ca.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	ca, err = x509.ParseCertificate(der)
	require.NoError(t, err)
	secret.Data[corev1.TLSCertKey], secret.Data[corev1.TLSPrivateKeyKey], err = issueLeaf("custom-system", ca, key, now)
	require.NoError(t, err)
	leaf, err := singleCertificate(secret.Data[corev1.TLSCertKey])
	require.NoError(t, err)

	leaf.NotAfter = now.Add(365 * day)
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, leaf.PublicKey, key)
	require.NoError(t, err)

	secret.Data[corev1.TLSCertKey] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	delete(secret.Data, tlsStateKey)
	delete(secret.Data, previousCAKey)
	delete(secret.Annotations, tlsStateAnnotation)
	before := secret.DeepCopy()
	next, err := renewTLS(secret, "custom-system", now)
	require.EqualError(t, err, "missing Racer serving rotation state")
	require.Nil(t, next)
	require.Equal(t, before, secret, "unsupported credentials must not be silently replaced")
}

func TestTLSCorruptionFailsClosed(t *testing.T) {
	now := tlsEpoch()
	original, err := newTLS("custom-system", now)
	require.NoError(t, err)
	rotated, err := renewTLS(original, "custom-system", now.Add(caRotationInterval))
	require.NoError(t, err)
	other, err := newTLS("custom-system", now)
	require.NoError(t, err)

	for name, damage := range map[string]func(*corev1.Secret){
		"missing-state": func(s *corev1.Secret) { delete(s.Data, tlsStateKey) },
		"missing-bundle-and-state": func(s *corev1.Secret) {
			delete(s.Data, caBundleKey)
			delete(s.Data, tlsStateKey)
		},
		"all-metadata-missing": func(s *corev1.Secret) {
			delete(s.Data, tlsStateKey)
			delete(s.Data, previousCAKey)
			delete(s.Annotations, tlsStateAnnotation)
		},
		"missing-marker":   func(s *corev1.Secret) { delete(s.Annotations, tlsStateAnnotation) },
		"missing-previous": func(s *corev1.Secret) { delete(s.Data, previousCAKey) },
		"bad-state":        func(s *corev1.Secret) { s.Data[tlsStateKey] = []byte("{") },
		"trailing-state":   func(s *corev1.Secret) { s.Data[tlsStateKey] = append(s.Data[tlsStateKey], []byte(" {}")...) },
		"wrong-ca-key":     func(s *corev1.Secret) { s.Data["ca.key"] = other.Data["ca.key"] },
		"foreign-leaf": func(s *corev1.Secret) {
			s.Data[corev1.TLSCertKey] = other.Data[corev1.TLSCertKey]
			s.Data[corev1.TLSPrivateKeyKey] = other.Data[corev1.TLSPrivateKeyKey]
		},
		"ca-bundle": func(s *corev1.Secret) { s.Data["ca.crt"] = append(s.Data["ca.crt"], other.Data["ca.crt"]...) },
		"missing-chain": func(s *corev1.Secret) {
			block, _ := pem.Decode(s.Data[corev1.TLSCertKey])
			s.Data[corev1.TLSCertKey] = pem.EncodeToMemory(block)
		},
		"wrong-root": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Previous[0].Root = other.Data["ca.crt"]
			require.NoError(t, writeTLSState(s, state))
		},
		"wrong-cross": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Previous[0].Cross = other.Data["ca.crt"]
			require.NoError(t, writeTLSState(s, state))
		},
		"extended-overlap": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Previous[0].RetireAt = state.Previous[0].RetireAt.Add(day)
			require.NoError(t, writeTLSState(s, state))
		},
		"shortened-overlap": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Previous[0].RetireAt = state.Previous[0].RetireAt.Add(-day)
			require.NoError(t, writeTLSState(s, state))
		},
		"wrong-created": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.CreatedAt = state.CreatedAt.Add(day)
			require.NoError(t, writeTLSState(s, state))
		},
		"too-many-generations": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Previous = append(state.Previous, state.Previous[0], state.Previous[0])
			data, err := json.Marshal(state)
			require.NoError(t, err)

			s.Data[tlsStateKey] = data
		},
		"unknown-version": func(s *corev1.Secret) {
			state := stateOf(t, s)
			state.Version = 2
			require.NoError(t, writeTLSState(s, state))
		},
		"deleted": func(s *corev1.Secret) { s.DeletionTimestamp = &metav1.Time{Time: now}; s.Finalizers = []string{"test"} },
	} {
		t.Run(name, func(t *testing.T) {
			secret := rotated.DeepCopy()
			damage(secret)
			env := testEnv(t, secret)
			plan := component.NewPlan()
			result, err := planTLSAt(t.Context(), env, plan, false, now.Add(2*caRotationInterval))
			require.Error(t, err)
			require.Nil(t, result)
			require.Zero(t, plan.Len())
		})
	}
}

func TestTLSCASAndLostResponse(t *testing.T) {
	for _, scenario := range []string{"concurrent-winner", "lost-response", "forbidden"} {
		t.Run(scenario, func(t *testing.T) {
			now := tlsEpoch()
			secret, err := newTLS("custom-system", now)
			require.NoError(t, err)
			env := testEnv(t, secret)
			at := now.Add(caRotationInterval)
			first, second := component.NewPlan(), component.NewPlan()
			result, err := planTLSAt(t.Context(), env, first, false, at)
			require.NoError(t, err)
			require.Nil(t, result)
			result, err = planTLSAt(t.Context(), env, second, false, at)
			require.NoError(t, err)
			require.Nil(t, result)
			require.Len(t, first.Operations, 1)
			require.Equal(t, component.OpMergePatch, first.Operations[0].Kind)

			if scenario == "concurrent-winner" {
				persist(t, env, first)
			}

			executor := *env
			if scenario != "concurrent-winner" {
				executor.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if scenario == "forbidden" {
							return apierrors.NewForbidden(corev1.Resource("secrets"), tlsName, errors.New("denied"))
						}

						require.NoError(t, c.Patch(ctx, obj, patch, opts...))

						return errors.New("lost write response")
					},
				})
			}

			executed, err := executor.Execute(t.Context(), second)
			require.NoError(t, err)

			if scenario == "concurrent-winner" {
				require.Len(t, executed.DeferredResults(), 1)
			} else {
				require.Error(t, executed.Err())
			}

			stored := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))

			restart := component.NewPlan()
			result, err = planTLSAt(t.Context(), env, restart, false, at)
			require.NoError(t, err)

			if scenario == "forbidden" {
				require.Nil(t, result)
				require.Equal(t, secret.Data, stored.Data)
				require.Len(t, restart.Operations, 1)
			} else {
				require.Equal(t, stored.Data, result.Data)
				require.Zero(t, restart.Len())
				require.NoError(t, verifyServing(t, result, secret.Data["ca.crt"], at))
			}
		})
	}
}

func TestTLSRotationDoesNotChangePodTemplates(t *testing.T) {
	for _, legacyHash := range []string{"", "legacy-hash-preserved"} {
		t.Run(legacyHash, func(t *testing.T) {
			env := testEnv(t, cache("cache"))
			initialize(t, env)
			persist(t, env, planPass(t, env))

			deployment, ds := &appsv1.Deployment{}, &appsv1.DaemonSet{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))

			if legacyHash != "" {
				deployment.Spec.Template.Annotations["unbounded-cloud.io/racer-tls-hash"] = legacyHash
				require.NoError(t, env.Client.Update(t.Context(), deployment))
			}

			// Compatibility metadata is removed on reconciliation, independently
			// of TLS rotation. An old installation may roll once for this change.
			persist(t, env, planPass(t, env))
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
			require.NotContains(t, deployment.Spec.Template.Annotations, "unbounded-cloud.io/racer-tls-hash")

			beforeController, beforeDataplane := deployment.Spec.Template.DeepCopy(), ds.Spec.Template.DeepCopy()
			now := time.Now().UTC().Truncate(time.Second)
			old, err := newTLS(env.Namespace, now.Add(-caRotationInterval))
			require.NoError(t, err)

			stored := &corev1.Secret{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))
			stored.Data = old.Data
			require.NoError(t, env.Client.Update(t.Context(), stored))
			rotation := planPass(t, env)
			require.Len(t, rotation.Operations, 1, "persist TLS before publishing derived trust")
			persist(t, env, rotation)
			persist(t, env, planPass(t, env))
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, controllerName), deployment))
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, dataplaneName), ds))
			require.Equal(t, *beforeController, deployment.Spec.Template)
			require.Equal(t, *beforeDataplane, ds.Spec.Template)
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, tlsName), stored))

			trust := &corev1.ConfigMap{}
			require.NoError(t, env.Client.Get(t.Context(), objectKey(env, trustName), trust))
			require.Equal(t, string(stored.Data["ca.crt"])+string(stored.Data[previousCAKey]), trust.Data["ca.crt"])
			require.NoError(t, verifyServing(t, stored, []byte(trust.Data["ca.crt"]), now))
			require.NoError(t, verifyServing(t, old, []byte(trust.Data["ca.crt"]), now), "old live server remains trusted during projection delay")
		})
	}
}
