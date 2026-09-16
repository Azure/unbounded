// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"context"
	"fmt"
	"net/url"

	"k8s.io/client-go/kubernetes"
)

type statusRequest func(context.Context, string, string, []byte) ([]byte, error)

// newStatusRequest reuses kubectl credentials and authenticated port-forward fallback.
// Each HTTP attempt is bounded independently; the caller owns the overall deadline.
func newStatusRequest(rt *pluginRuntime, opts nodeStatusFetchOptions) (statusRequest, error) {
	ns, err := rt.namespace()
	if err != nil {
		return nil, err
	}

	client, err := rt.kubeClient()
	if err != nil {
		return nil, err
	}

	cfg, err := rt.restConfig()
	if err != nil {
		return nil, err
	}

	return func(ctx context.Context, method, path string, body []byte) ([]byte, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, opts.timeout)
		defer cancel()

		raw, err := requestStatusViaAggregatedAPI(attemptCtx, client, method, path, body)
		if err == nil || len(raw) > 0 || ctx.Err() != nil {
			return raw, err
		}

		fallbackCtx, fallbackCancel := context.WithTimeout(ctx, opts.timeout)
		defer fallbackCancel()

		return requestStatusViaPortForward(fallbackCtx, client, cfg, ns,
			opts.controllerDeploy, opts.controllerSelector, opts.controllerPort, opts.timeout,
			method, path, body)
	}, nil
}

func requestStatusViaAggregatedAPI(ctx context.Context, client *kubernetes.Clientset, method, path string, body []byte) ([]byte, error) {
	target, err := url.ParseRequestURI(path)
	if err != nil || target.IsAbs() || target.Host != "" {
		return nil, fmt.Errorf("invalid controller status path %q", path)
	}

	request := client.CoreV1().RESTClient().Verb(method).
		AbsPath("/apis/status.net.unbounded-cloud.io/v1alpha1" + target.Path)
	for key, values := range target.Query() {
		for _, value := range values {
			request.Param(key, value)
		}
	}

	if body != nil {
		request.SetHeader("Content-Type", "application/json").Body(body)
	}

	return request.DoRaw(ctx)
}
