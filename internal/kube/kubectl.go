// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"context"
	"os/exec"

	"github.com/Azure/unbounded/internal/helpers"
)

type KubectlFunc func(context.Context) *exec.Cmd

const (
	kubectlBinary = "kubectl"
)

func Kubectl(env []string, kubeconfig string) KubectlFunc {
	return func(ctx context.Context) *exec.Cmd {
		envMap := helpers.EnvSliceToMap(env)
		envMap["KUBECONFIG"] = kubeconfig

		c := exec.CommandContext(ctx, kubectlBinary)
		c.Env = helpers.EnvMapToSlice(envMap)

		return c
	}
}
