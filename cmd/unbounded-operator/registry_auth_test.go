// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Azure/unbounded/internal/registryauth"
)

func TestCredentialProviderCommandUsesOnlyProtocolStdout(t *testing.T) {
	command := newCommand(func(context.Context, config) error {
		t.Fatal("controller must not start for exec-provider invocation")
		return nil
	})
	command.SetArgs([]string{"acr-credential-provider", "--registry=example.azurecr.io"})
	command.SetIn(strings.NewReader(`{"kind":"CredentialProviderRequest","apiVersion":"credentialprovider.kubelet.k8s.io/v1","image":"docker.io/library/busybox:latest"}`))

	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	require.NoError(t, command.Execute())

	var response registryauth.ProviderResponse
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &response))
	require.Empty(t, response.Auth)
	require.Empty(t, stderr.String())
}

func TestCredentialProviderCommandRejectsVersionWithoutUsageOnStdout(t *testing.T) {
	command := newCommand(func(context.Context, config) error { return nil })
	command.SetArgs([]string{"acr-credential-provider"})
	command.SetIn(strings.NewReader(`{"kind":"CredentialProviderRequest","apiVersion":"unsupported"}`))

	var stdout, stderr bytes.Buffer
	command.SetOut(&stdout)
	command.SetErr(&stderr)
	require.ErrorContains(t, command.Execute(), "unsupported")
	require.Empty(t, stdout.String())
}
