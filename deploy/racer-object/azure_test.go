// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject_test

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

func TestAzureOriginExample(t *testing.T) {
	var azure, s3 appsv1.DaemonSet
	readExample(t, "origin-azure.yaml", &azure)
	readExample(t, "origin.yaml", &s3)

	pod := azure.Spec.Template.Spec
	require.Len(t, pod.Containers, 1)
	checkPod(t, pod)

	origin := pod.Containers[0]
	require.Equal(t, []string{
		"origin", "--volume=racer-object", "--namespace=example-azure-store", "--bucket=example-container",
		"--provider=azure", "--account-url=https://ACCOUNT.blob.core.windows.net", "--azure-credential=default",
		"--metadata-ttl=30s", "--request-timeout=1m",
	}, origin.Args)
	require.Empty(t, origin.EnvFrom)
	require.Len(t, origin.Env, 1)
	require.Equal(t, "AZURE_CLIENT_ID", origin.Env[0].Name)
	require.Empty(t, origin.Env[0].Value)

	optional := true
	require.Equal(t, &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "racer-object-azure"},
		Key:                  "AZURE_CLIENT_ID",
		Optional:             &optional,
	}}, origin.Env[0].ValueFrom)
	checkSocket(t, pod, origin, "origin", corev1.HostPathDirectoryOrCreate, false)

	// Only provider settings differ; keep placement, security, and mounts intact.
	azure.Spec.Template.Spec.Containers[0].Args = s3.Spec.Template.Spec.Containers[0].Args
	azure.Spec.Template.Spec.Containers[0].Env = s3.Spec.Template.Spec.Containers[0].Env
	azure.Spec.Template.Spec.Containers[0].EnvFrom = s3.Spec.Template.Spec.Containers[0].EnvFrom
	require.Equal(t, s3, azure)
}

func TestRacerObjectHelpDescribesBothProviders(t *testing.T) {
	data, err := os.ReadFile("../../Makefile")
	require.NoError(t, err)

	for _, target := range []string{"racer-object", "racer-object-build", "racer-object-test", "image-racer-object-local"} {
		t.Run(target, func(t *testing.T) {
			var helpLine, targetLine string

			for line := range strings.SplitSeq(string(data), "\n") {
				if strings.HasPrefix(line, "\t@echo \"  "+target+" ") {
					helpLine = line
				}

				if strings.HasPrefix(line, target+":") {
					targetLine = line
				}
			}

			require.Contains(t, helpLine, "S3 and Azure Blob read adapter")
			require.Contains(t, targetLine, "S3 and Azure Blob read adapter")
		})
	}
}
