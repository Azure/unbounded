// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	manifests "github.com/Azure/unbounded/deploy/racer"
	"github.com/Azure/unbounded/internal/operator/component"
	"github.com/Azure/unbounded/internal/racer/workload"
)

const (
	dataplaneName          = workload.DataplaneDaemonSetName
	dataplaneConfigName    = "racer-settings"
	dataplaneImageName     = "racer"
	objectImageName        = "racer-object"
	dataplaneContainerName = workload.ContainerName
	dataplaneConfigHash    = "unbounded-cloud.io/racer-settings-hash"

	// minThreads covers one I/O worker, one crypto helper, and the
	// dedicated control plane thread. It applies only when
	// RACER_ENABLE_RDMA is false.
	minThreads = 3

	// minThreadsRDMA adds the RDMA owner thread, which the dataplane
	// reserves whenever RACER_ENABLE_RDMA is auto (with hardware present)
	// or true.
	minThreadsRDMA = minThreads + 1
)

// planDataplaneConfig creates racer-settings when it is absent, seeded
// from the owned pre-rename racer-dataplane-config or else with defaults, and otherwise reads it. Every key is administrator payload; its
// hash rolls the DaemonSet.
func planDataplaneConfig(ctx context.Context, env *component.Env, plan *component.Plan) (*corev1.ConfigMap, error) {
	decoded, err := env.DecodeManifestFiles(manifests.Manifests, []string{"dataplane-config.yaml"}, nil)
	if err != nil {
		return nil, err
	}

	if len(decoded) != 1 {
		return nil, errors.New("racer dataplane config manifest must contain one object")
	}

	config := &corev1.ConfigMap{}

	err = env.LiveReader().Get(ctx, client.ObjectKeyFromObject(decoded[0]), config)
	if apierrors.IsNotFound(err) {
		if err := env.Scheme.Convert(decoded[0], config, nil); err != nil {
			return nil, err
		}

		config.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		markManaged(config)

		// Carry settings over from the pre-rename ConfigMap the operator owns.
		renamed, err := renamedConfigData(ctx, env)
		if err != nil {
			return nil, err
		}

		if renamed != nil {
			// Drop retired keys the earlier template seeded with defaults.
			renamed, _ = migrateRetiredSettings(renamed)

			// Reject bad carried-over values before the new DaemonSet rolls
			// out, as for an existing racer-settings.
			if err := validateDataplaneConfig(renamed); err != nil {
				return nil, err
			}

			config.Data = renamed
		}

		plan.Add(component.Operation{Kind: component.OpCreateIfAbsent, Object: component.ToUnstructured(config), Component: name})

		return config, nil
	}

	if err != nil {
		return nil, err
	}

	config.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("ConfigMap"))

	if err := validateAdoptable(ctx, env, config); err != nil {
		return nil, err
	}

	// Earlier templates seeded retired keys with defaults the dataplane now
	// rejects. Drop them and write the result back, so Pods never import the
	// old keys through envFrom. The patch carries the observed
	// resourceVersion, so a concurrent edit conflicts and the pass retries.
	if migrated, changed := migrateRetiredSettings(config.Data); changed {
		observed := config.Data

		if err := validateDataplaneConfig(migrated); err != nil {
			return nil, err
		}

		config.Data = migrated
		ref := component.RefOf(component.ToUnstructured(config))

		// Legacy migration may already patch this ConfigMap from the same
		// resourceVersion to drop its stamp. Fold the data into that patch,
		// since a second patch from the same version would conflict.
		for i := range plan.Operations {
			op := &plan.Operations[i]
			if op.Kind == component.OpMergePatch && op.Ref() == ref {
				if err := unstructured.SetNestedStringMap(op.Object.Object, migrated, "data"); err != nil {
					return nil, err
				}

				return config, nil
			}
		}

		base := config.DeepCopy()
		base.Data = observed

		plan.Add(component.Operation{Kind: component.OpMergePatch, Object: component.ToUnstructured(config), Base: component.ToUnstructured(base), Component: name})

		return config, nil
	}

	if err := validateDataplaneConfig(config.Data); err != nil {
		return nil, err
	}

	return config, nil
}

// planDataplane adds the dataplane ServiceAccount and DaemonSet. Both record
// the racer-state UID they run on. The DaemonSet waits for every listed
// dependency, which includes its RBAC bindings.
func planDataplane(ctx context.Context, env *component.Env, plan *component.Plan, config *corev1.ConfigMap, stateUID string, dependencies []component.ObjectRef) error {
	account := &corev1.ServiceAccount{
		TypeMeta:                     metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta:                   metav1.ObjectMeta{Name: dataplaneName, Namespace: env.Namespace, Annotations: map[string]string{stateUIDAnnotation: stateUID}},
		AutomountServiceAccountToken: ptr.To(false),
	}

	op, err := ownedOperation(ctx, env, component.Operation{Kind: component.OpApply, Object: component.ToUnstructured(account), Component: name})
	if err != nil {
		return err
	}

	plan.Add(op)

	cfg := workload.DefaultConfig(env.Namespace, env.Config.Image(dataplaneImageName), env.Config.Image(objectImageName))
	// The NVIDIA mode changes the Pod shape, not only its environment, so it
	// is read here rather than left to the ConfigMap import.
	cfg.NVIDIA = workload.NVIDIAInjection(config.Data[workload.NVIDIAKey])

	ds, err := workload.DaemonSet(cfg)
	if err != nil {
		return err
	}

	ds.Annotations = map[string]string{stateUIDAnnotation: stateUID}
	if ds.Spec.Template.Annotations == nil {
		ds.Spec.Template.Annotations = map[string]string{}
	}

	ds.Spec.Template.Annotations[dataplaneConfigHash] = component.ConfigMapPayloadHash(config)
	ds.Spec.Template.Annotations[nativeOriginAnnotation] = "true"

	container := &ds.Spec.Template.Spec.Containers[0]
	// Explicit env wiring wins over these imports.
	container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: dataplaneConfigName}}}}

	dependsOn := append(append([]component.ObjectRef(nil), dependencies...), op.Ref())

	// The Pod template hash covers the migrated payload, so roll Pods only
	// after the ConfigMap patch that writes it lands.
	configRef := component.RefOf(component.ToUnstructured(config))
	for _, planned := range plan.Operations {
		if planned.Kind == component.OpMergePatch && planned.Ref() == configRef {
			dependsOn = append(dependsOn, configRef)

			break
		}
	}

	op, err = ownedOperation(ctx, env, component.Operation{
		Kind: component.OpApply, Object: component.ToUnstructured(ds), Component: name,
		Overridable: true, ValidateOverride: ValidateDataplaneOverride,
		DependsOn: dependsOn,
	})
	if err != nil {
		return err
	}

	plan.Add(op)

	return nil
}

// recordedStateUID returns the racer-state UID an existing installation runs
// on, or empty when no managed dataplane object records one. The retired
// controller's DaemonSet records none, so migration starts a new identity.
// The pre-rename DaemonSet and ServiceAccount are read too, until
// planRenamedCleanup removes them, so the rename keeps failing closed.
func recordedStateUID(ctx context.Context, env *component.Env) (string, error) {
	var uid string

	for _, objName := range []string{dataplaneName, renamedDataplane} {
		for _, obj := range []client.Object{&corev1.ServiceAccount{}, &unstructured.Unstructured{Object: map[string]any{"apiVersion": "apps/v1", "kind": "DaemonSet"}}} {
			err := env.LiveReader().Get(ctx, objectKey(env, objName), obj)
			if apierrors.IsNotFound(err) {
				continue
			}

			if err != nil {
				return "", err
			}

			// A terminating object still records the identity its Pods may be
			// running on, so it keeps failing closed until it is gone.
			if obj.GetUID() == "" || (obj.GetAnnotations()[managerAnnotation] != component.FieldOwner && obj.GetLabels()[managerLabel] != component.FieldOwner) {
				continue
			}

			recorded := obj.GetAnnotations()[stateUIDAnnotation]
			if recorded == "" {
				continue
			}

			if uid != "" && uid != recorded {
				return "", fmt.Errorf("racer dataplane ServiceAccounts and DaemonSets (%s, %s) record different racer-state UIDs", dataplaneName, renamedDataplane)
			}

			uid = recorded
		}
	}

	return uid, nil
}

// ValidateDataplaneOverride permits scheduling, rollout, resource, and extra
// environment tuning, but keeps identity, credentials, and runtime wiring
// canonical.
func ValidateDataplaneOverride(original, candidate *unstructured.Unstructured) error {
	container := func(ds *unstructured.Unstructured) (map[string]any, int) {
		var found map[string]any

		count := 0

		containers, _, err := unstructured.NestedSlice(ds.Object, "spec", "template", "spec", "containers")
		if err != nil {
			return nil, 0
		}

		for _, entry := range containers {
			c, ok := entry.(map[string]any)
			if ok && c["name"] == dataplaneContainerName {
				found = c
				count++
			}
		}

		return found, count
	}

	before, beforeCount := container(original)

	after, afterCount := container(candidate)
	if beforeCount != 1 || afterCount != 1 {
		return fmt.Errorf("racer DaemonSet must retain exactly one canonical dataplane container; tune ConfigMap %s instead", dataplaneConfigName)
	}

	for _, field := range []string{"envFrom", "volumeMounts"} {
		if !reflect.DeepEqual(before[field], after[field]) {
			return fmt.Errorf("racer DaemonSet container dataplane %s must match the canonical configuration; tune ConfigMap %s instead", field, dataplaneConfigName)
		}
	}

	containerEnv := func(c map[string]any) ([]any, error) {
		raw, present := c["env"]
		if !present || raw == nil {
			return nil, nil
		}

		envs, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("racer DaemonSet container dataplane env must be a list, got %T", raw)
		}

		return envs, nil
	}

	afterEnvs, err := containerEnv(after)
	if err != nil {
		return err
	}

	afterEnv := map[string]any{}

	for _, entry := range afterEnvs {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		envName := fmt.Sprint(e["name"])
		if _, dup := afterEnv[envName]; dup {
			return fmt.Errorf("racer DaemonSet container dataplane env %s is set more than once; tune ConfigMap %s instead", envName, dataplaneConfigName)
		}

		if envName == workload.ControlSeatsKey {
			return fmt.Errorf("racer DaemonSet container dataplane env %s would shadow ConfigMap %s; tune it there instead", envName, dataplaneConfigName)
		}

		afterEnv[envName] = e
	}

	beforeEnvs, err := containerEnv(before)
	if err != nil {
		return err
	}

	for _, entry := range beforeEnvs {
		e, ok := entry.(map[string]any)
		if !ok {
			continue
		}

		if !reflect.DeepEqual(afterEnv[fmt.Sprint(e["name"])], e) {
			return fmt.Errorf("racer DaemonSet container dataplane env %v is operator-owned wiring and cannot be overridden; tune ConfigMap %s instead", e["name"], dataplaneConfigName)
		}
	}

	for _, field := range []string{"serviceAccountName", "automountServiceAccountToken", "volumes"} {
		beforeValue, _, err := unstructured.NestedFieldNoCopy(original.Object, "spec", "template", "spec", field)
		if err != nil {
			return fmt.Errorf("reading canonical racer DaemonSet pod %s: %w", field, err)
		}

		afterValue, _, err := unstructured.NestedFieldNoCopy(candidate.Object, "spec", "template", "spec", field)
		if err != nil {
			return fmt.Errorf("reading racer DaemonSet override pod %s: %w", field, err)
		}

		if !reflect.DeepEqual(beforeValue, afterValue) {
			return fmt.Errorf("racer DaemonSet pod %s is operator-owned wiring and cannot be overridden; tune ConfigMap %s instead", field, dataplaneConfigName)
		}
	}

	return nil
}
