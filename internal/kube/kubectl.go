// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package kube

import (
	"context"
	"os/exec"
	"slices"
	"strings"
)

type KubectlFunc func(context.Context) *exec.Cmd

const (
	kubectlBinary = "kubectl"
)

func Kubectl(env []string, kubeconfig string) KubectlFunc {
	return func(ctx context.Context) *exec.Cmd {
		c := exec.CommandContext(ctx, kubectlBinary)

		c.Env = append(slices.DeleteFunc(slices.Clone(env), func(v string) bool {
			return strings.HasPrefix(v, "KUBECONFIG=")
		}), "KUBECONFIG="+kubeconfig)

		return c
	}
}
