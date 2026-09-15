// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"k8s.io/client-go/rest"
)

const deprecatedEndpointsWarning = "v1 Endpoints is deprecated in v1.33+; use discovery.k8s.io/v1 EndpointSlice"

type endpointWarningHandler struct {
	delegate rest.WarningHandlerWithContext
}

func newEndpointWarningHandler() endpointWarningHandler {
	return endpointWarningHandler{delegate: rest.WarningLogger{}}
}

func (h endpointWarningHandler) HandleWarningHeaderWithContext(ctx context.Context, code int, agent, message string) {
	if code == 299 && message == deprecatedEndpointsWarning {
		return
	}

	h.delegate.HandleWarningHeaderWithContext(ctx, code, agent, message)
}
