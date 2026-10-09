// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Azure/unbounded/internal/operator/component"
	racercore "github.com/Azure/unbounded/internal/racer"
	"github.com/Azure/unbounded/internal/racer/authority"
	"github.com/Azure/unbounded/internal/racer/wire"
)

func bootstrapPlan(t *testing.T, env *component.Env) *component.Plan {
	t.Helper()

	for range 10 {
		plan := planPass(t, env)
		for _, op := range plan.Operations {
			if op.Kind == component.OpRun {
				require.Len(t, plan.Operations, 1)
				return plan
			}

			require.NotEqual(t, "Deployment", op.Object.GetKind())
			require.NotEqual(t, "RoleBinding", op.Object.GetKind())
			require.NotEqual(t, "ClusterRoleBinding", op.Object.GetKind())
		}

		persist(t, env, plan)
	}

	t.Fatal("bootstrap operation missing")

	return nil
}

func TestBootstrapCompleteBeforeRuntimeAndReplayReadOnly(t *testing.T) {
	env := testEnv(t, cacheObject("one"), cacheObject("two"))
	plan := bootstrapPlan(t, env)
	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	cm.Data["RACER_CERTIFICATE_LIFETIME"] = "2m"
	cm.Data["RACER_ROTATION_INTERVAL"] = "5m"
	cm.Data["RACER_ROTATION_PREPARE_FOR"] = "20s"
	cm.Data["RACER_ROTATION_RETAIN_FOR"] = "2m"
	require.NoError(t, env.Client.Update(t.Context(), cm))
	result, err := env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.ErrorContains(t, result.Err(), "configuration changed")
	plan = bootstrapPlan(t, env)
	persist(t, env, plan)

	secret := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), secret))
	require.Len(t, secret.Data, 3)
	bundle, err := wire.DecodeBundle(bytes.NewReader(secret.Data["bundle.json"]))
	require.NoError(t, err)

	cacheIDs := map[wire.CacheID]bool{}
	for _, key := range bundle.CacheKeys {
		cacheIDs[key.Key.Cache] = true
	}

	require.Len(t, cacheIDs, 2)

	var rotation struct {
		NextRotation time.Time `json:"next_rotation"`
	}
	require.NoError(t, json.Unmarshal(secret.Data["rotation.json"], &rotation))
	require.WithinDuration(t, time.Now().Add(5*time.Minute-20*time.Second), rotation.NextRotation, 5*time.Second)

	version := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), version))
	require.Equal(t, string(secret.UID), version.Annotations[credentialsUID])

	cfg, err := racercore.ConfigFromLookup(func(key string) (string, bool) {
		if key == "POD_NAMESPACE" {
			return env.Namespace, true
		}

		value, ok := cm.Data[key]

		return value, ok
	})
	require.NoError(t, err)

	before := secret.DeepCopy()
	original := env.Client
	env.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("runtime or stale bootstrap created state")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("runtime or stale bootstrap updated state")
			return nil
		},
	})
	app := racercore.Assemble(cfg, env.Client, env.LiveReader())
	require.NoError(t, app.Recover(t.Context(), env.Client))
	_, err = app.Keyring.Reconcile(t.Context(), ctrl.Request{})
	require.NoError(t, err)
	persist(t, env, plan)
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), secret))
	require.Equal(t, before, secret)

	env.Client = original
	require.NoError(t, env.Client.Delete(t.Context(), secret))
	_, _, err = (Component{}).Plan(t.Context(), env, nil)
	require.Error(t, err)
	result, err = env.Execute(t.Context(), plan)
	require.NoError(t, err)
	require.Error(t, result.Err())
}

func TestBootstrapInterruptionsResumeExactCandidate(t *testing.T) {
	for _, boundary := range []string{"version-create", "marker-update", "secret-create", "version-update"} {
		for _, persisted := range []bool{false, true} {
			t.Run(boundary+map[bool]string{false: "/before", true: "/after"}[persisted], func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				plan := bootstrapPlan(t, env)
				original := env.Client
				boom := errors.New("bootstrap interrupted")
				write := func(match bool, f func() error) error {
					if match && !persisted {
						return boom
					}

					if err := f(); err != nil {
						return err
					}

					if match {
						return boom
					}

					return nil
				}
				env.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						match := boundary == "version-create" && obj.GetName() == versionName || boundary == "secret-create" && obj.GetName() == credentialsName
						return write(match, func() error { return c.Create(ctx, obj, opts...) })
					},
					Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						match := boundary == "marker-update" && obj.GetName() == markerName || boundary == "version-update" && obj.GetName() == versionName
						return write(match, func() error { return c.Update(ctx, obj, opts...) })
					},
				})
				result, err := env.Execute(t.Context(), plan)
				require.NoError(t, err)
				require.ErrorIs(t, result.Err(), boom)

				before := &corev1.Secret{}
				hadSecret := original.Get(t.Context(), objectKey(env, credentialsName), before) == nil
				env.Client = original
				persist(t, env, plan)

				after := &corev1.Secret{}
				require.NoError(t, original.Get(t.Context(), objectKey(env, credentialsName), after))

				if hadSecret {
					require.Equal(t, before, after)
				}
			})
		}
	}
}

func TestBootstrapExecutionRejectsInactiveAndReplacedIdentity(t *testing.T) {
	for _, target := range []string{"cache", "marker"} {
		t.Run(target, func(t *testing.T) {
			env := testEnv(t, cacheObject("cache"))

			plan := bootstrapPlan(t, env)
			if target == "cache" {
				require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
			} else {
				marker := &corev1.ConfigMap{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
				require.NoError(t, env.Client.Delete(t.Context(), marker))
				marker.ResourceVersion = ""
				require.NoError(t, env.Client.Create(t.Context(), marker))
			}

			result, err := env.Execute(t.Context(), plan)
			require.NoError(t, err)
			require.Error(t, result.Err())
			require.Error(t, env.Client.Get(t.Context(), objectKey(env, versionName), &corev1.ConfigMap{}))
		})
	}
}

func TestBootstrapWriterRejectsRotation(t *testing.T) {
	env := testEnv(t)
	w := bootstrapWriter{writer: env.Client, namespace: env.Namespace}
	require.Error(t, w.Update(t.Context(), &corev1.Secret{}))
	require.Error(t, w.Create(t.Context(), &corev1.ServiceAccount{}))
	require.Error(t, w.Update(t.Context(), &corev1.ConfigMap{}))
}

func TestBootstrapCompetingPlansAndRetention(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	first := bootstrapPlan(t, env)
	second := bootstrapPlan(t, env)
	persist(t, env, first)

	before := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), before))
	persist(t, env, second)
	persist(t, env, planPass(t, env))
	require.NoError(t, env.Client.Delete(t.Context(), cacheObject("cache")))
	require.Zero(t, planPass(t, env).Len())

	after := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), after))
	require.Equal(t, before, after)
	require.NoError(t, env.Client.Delete(t.Context(), after))
	require.Zero(t, planPass(t, env).Len())
	require.NoError(t, env.Client.Create(t.Context(), cacheObject("later")))
	_, _, err := (Component{}).Plan(t.Context(), env, nil)
	require.Error(t, err)
}

func TestBootstrapCancellationAfterCredentialCreate(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	plan := bootstrapPlan(t, env)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	original := env.Client
	env.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, obj, opts...); err != nil {
				return err
			}

			if obj.GetName() == credentialsName {
				cancel()
			}

			return nil
		},
	})
	result, err := env.Execute(ctx, plan)
	require.NoError(t, err)
	require.ErrorIs(t, result.Err(), context.Canceled)

	before := &corev1.Secret{}
	require.NoError(t, original.Get(t.Context(), objectKey(env, credentialsName), before))

	version := &corev1.ConfigMap{}
	require.NoError(t, original.Get(t.Context(), objectKey(env, versionName), version))
	require.Empty(t, version.Annotations[credentialsClaim])

	env.Client = original
	persist(t, env, plan)

	after := &corev1.Secret{}
	require.NoError(t, original.Get(t.Context(), objectKey(env, credentialsName), after))
	require.Equal(t, before, after)
}

func TestBootstrapLifetimeIncreaseAllowsRuntimeRollout(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	bootstrapPlan(t, env)

	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	cm.Data["RACER_CERTIFICATE_LIFETIME"] = "2m"
	cm.Data["RACER_ROTATION_INTERVAL"] = "5m"
	cm.Data["RACER_ROTATION_PREPARE_FOR"] = "20s"
	cm.Data["RACER_ROTATION_RETAIN_FOR"] = "2m"
	require.NoError(t, env.Client.Update(t.Context(), cm))
	stale := bootstrapPlan(t, env)
	persist(t, env, stale)

	before := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), before))

	for _, key := range []string{"RACER_CERTIFICATE_LIFETIME", "RACER_ROTATION_INTERVAL", "RACER_ROTATION_PREPARE_FOR", "RACER_ROTATION_RETAIN_FOR"} {
		delete(cm.Data, key)
	}

	require.NoError(t, env.Client.Update(t.Context(), cm))
	cfg, err := configAuthority(env, cm)
	require.NoError(t, err)

	owner := authority.New(cfg, authority.Dependencies{Reader: env.LiveReader()})
	require.Error(t, owner.Observe(t.Context()), "regression requires signing readiness to fail")
	plan := planPass(t, env)
	kinds := map[string]bool{}

	for _, op := range plan.Operations {
		require.NotEqual(t, component.OpRun, op.Kind)
		kinds[op.Object.GetKind()] = true
	}

	for _, kind := range []string{"Deployment", "RoleBinding", "ClusterRoleBinding"} {
		require.True(t, kinds[kind], kind)
	}

	persist(t, env, plan)

	marker := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, markerName), marker))
	require.NoError(t, bootstrapAuthority(t.Context(), env, marker, cm), "committed action must not rotate or require signing readiness")

	after := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), after))
	require.Equal(t, before, after)
	delete(after.Data, "issuer.json")
	require.NoError(t, env.Client.Update(t.Context(), after))
	_, _, err = (Component{}).Plan(t.Context(), env, nil)
	require.Error(t, err, "corrupt credentials must still withhold runtime")
	require.Error(t, bootstrapAuthority(t.Context(), env, marker, cm))
}

func TestBootstrapExpiredIssuerAllowsRuntimeRepairWithoutRotation(t *testing.T) {
	env := testEnv(t, cacheObject("cache"))
	stale := bootstrapPlan(t, env)
	cm := &corev1.ConfigMap{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))
	cfg, err := configAuthority(env, cm)
	require.NoError(t, err)

	past := time.Now().Add(-30 * 24 * time.Hour)
	installer := authority.New(cfg, authority.Dependencies{Reader: env.LiveReader(), Writer: env.Client, Now: func() time.Time { return past }})
	require.NoError(t, installer.Recover(t.Context(), env.Client))
	_, err = installer.ReconcileCredentials(t.Context())
	require.NoError(t, err)

	observer := authority.New(cfg, authority.Dependencies{Reader: env.LiveReader()})
	require.Error(t, observer.Observe(t.Context()))

	before := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), before))
	original := env.Client
	env.Client = interceptor.NewClient(original.(client.WithWatch), interceptor.Funcs{
		Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
			t.Fatal("stale action created state")
			return nil
		},
		Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
			t.Fatal("stale action rotated expired issuer")
			return nil
		},
	})
	persist(t, env, stale)
	env.Client = original
	plan := planPass(t, env)
	kinds := map[string]bool{}

	for _, op := range plan.Operations {
		require.NotEqual(t, component.OpRun, op.Kind)
		kinds[op.Object.GetKind()] = true
	}

	for _, kind := range []string{"Deployment", "RoleBinding", "ClusterRoleBinding"} {
		require.True(t, kinds[kind], kind)
	}

	persist(t, env, plan)

	after := &corev1.Secret{}
	require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), after))
	require.Equal(t, before, after)
}

func TestBootstrapConfigPassValidatesDurableState(t *testing.T) {
	for _, configState := range []string{"unchanged", "missing", "drifted"} {
		for _, damage := range []string{"none", "version", "credentials", "commitment"} {
			t.Run(configState+"/"+damage, func(t *testing.T) {
				env := testEnv(t, cacheObject("cache"))
				initialize(t, env)

				cm := &corev1.ConfigMap{}
				require.NoError(t, env.Client.Get(t.Context(), objectKey(env, configName), cm))

				switch configState {
				case "missing":
					require.NoError(t, env.Client.Delete(t.Context(), cm))
				case "drifted":
					cm.Data["RACER_CREDENTIALS_SECRET_NAME"] = "wrong"
					require.NoError(t, env.Client.Update(t.Context(), cm))
				}

				switch damage {
				case "version", "commitment":
					version := &corev1.ConfigMap{}
					require.NoError(t, env.Client.Get(t.Context(), objectKey(env, versionName), version))

					if damage == "version" {
						version.Data["sequence"] = "0"
					} else {
						delete(version.Annotations, credentialsUID)
					}

					require.NoError(t, env.Client.Update(t.Context(), version))
				case "credentials":
					secret := &corev1.Secret{}
					require.NoError(t, env.Client.Get(t.Context(), objectKey(env, credentialsName), secret))
					delete(secret.Data, "issuer.json")
					require.NoError(t, env.Client.Update(t.Context(), secret))
				}

				versionReads := 0
				env.APIReader = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if key == objectKey(env, versionName) {
							versionReads++
						}

						return c.Get(ctx, key, obj, opts...)
					},
				})
				env.Client = interceptor.NewClient(env.Client.(client.WithWatch), interceptor.Funcs{
					Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
						t.Fatal("bootstrap planning created state")
						return nil
					},
					Update: func(context.Context, client.WithWatch, client.Object, ...client.UpdateOption) error {
						t.Fatal("bootstrap planning updated state")
						return nil
					},
					Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
						t.Fatal("bootstrap planning patched state")
						return nil
					},
				})

				plan, _, err := (Component{}).Plan(t.Context(), env, nil)
				if damage != "none" {
					require.Error(t, err)
					require.Nil(t, plan)

					return
				}

				require.NoError(t, err)
				// Recover reads twice, commitment once, persisted credentials once.
				require.Equal(t, 4, versionReads, "planning must not repeat authority recovery")

				if configState != "unchanged" {
					require.Len(t, plan.Operations, 1)
					require.Equal(t, configName, plan.Operations[0].Object.GetName())
				} else {
					require.Contains(t, plan.Summary(), "Deployment/")
				}

				for _, op := range plan.Operations {
					require.NotEqual(t, component.OpRun, op.Kind)
				}
			})
		}
	}
}
