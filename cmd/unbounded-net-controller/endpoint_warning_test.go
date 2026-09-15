// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
)

type recordedWarning struct {
	code    int
	agent   string
	message string
}

type recordingWarningHandler struct {
	warnings []recordedWarning
}

func (h *recordingWarningHandler) HandleWarningHeaderWithContext(_ context.Context, code int, agent, message string) {
	h.warnings = append(h.warnings, recordedWarning{code: code, agent: agent, message: message})
}

func TestEndpointWarningHandlerSuppressesOnlyEndpointsDeprecation(t *testing.T) {
	delegate := &recordingWarningHandler{}
	handler := endpointWarningHandler{delegate: delegate}
	ctx := context.Background()

	handler.HandleWarningHeaderWithContext(ctx, 299, "kube-apiserver", deprecatedEndpointsWarning)

	if len(delegate.warnings) != 0 {
		t.Fatalf("expected deprecated Endpoints warning to be suppressed, got %#v", delegate.warnings)
	}

	handler.HandleWarningHeaderWithContext(ctx, 299, "kube-apiserver", "another warning")
	handler.HandleWarningHeaderWithContext(ctx, 199, "kube-apiserver", deprecatedEndpointsWarning)

	if len(delegate.warnings) != 2 {
		t.Fatalf("expected unrelated warnings to be delegated, got %#v", delegate.warnings)
	}

	if got := delegate.warnings[0]; got.code != 299 || got.message != "another warning" {
		t.Fatalf("unexpected first delegated warning: %#v", got)
	}

	if got := delegate.warnings[1]; got.code != 199 || got.message != deprecatedEndpointsWarning {
		t.Fatalf("unexpected second delegated warning: %#v", got)
	}
}
