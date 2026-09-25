// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/Azure/unbounded/internal/operator/component"
)

func initializationJob(env *component.Env, uid types.UID) *batchv1.Job {
	return &batchv1.Job{
		TypeMeta: metav1.TypeMeta{APIVersion: "batch/v1", Kind: "Job"},
		ObjectMeta: metav1.ObjectMeta{
			Name: jobName, Namespace: env.Namespace,
			Annotations: map[string]string{"racer.unbounded-cloud.io/installation-uid": string(uid)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: ptr.To(int32(0)), ActiveDeadlineSeconds: ptr.To(int64(300)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				RestartPolicy: corev1.RestartPolicyNever, ServiceAccountName: controllerName,
				Containers: []corev1.Container{{
					Name: "initialize", Image: env.Config.Image(controllerName), Args: []string{"initialize"},
					EnvFrom: []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: configName}}}},
					Env:     []corev1.EnvVar{{Name: "POD_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}}},
				}},
			}},
		},
	}
}
