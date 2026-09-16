// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package net

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"

	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
)

type statusRequest func(context.Context, string, string, []byte) ([]byte, error)

type nodeDetailClient struct {
	request      statusRequest
	pollInterval time.Duration
}

func (c nodeDetailClient) fetch(ctx context.Context, nodeName string, forceRefresh bool) (*statusv1alpha1.NodeStatusResponse, error) {
	if len(validation.IsDNS1123Subdomain(nodeName)) != 0 {
		return nil, fmt.Errorf("invalid node name %q", nodeName)
	}

	body, err := json.Marshal(struct {
		ForceRefresh bool `json:"forceRefresh"`
	}{ForceRefresh: forceRefresh})
	if err != nil {
		return nil, err
	}

	path := "/status/node/" + nodeName + "/details"

	raw, requestErr := c.request(ctx, http.MethodPost, path, body)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	initial, err := decodeNodeDetailResult(raw, requestErr, nodeName, "")
	if err != nil {
		return nil, err
	}

	if initial.State == statusv1alpha1.NodeDetailComplete {
		return validateNodeDetails(initial, time.Now())
	}

	requestCtx, cancel := context.WithDeadline(ctx, initial.Deadline)
	defer cancel()

	for {
		interval := c.pollInterval
		if interval <= 0 {
			interval = time.Second
		}

		timer := time.NewTimer(interval)
		select {
		case <-requestCtx.Done():
			timer.Stop()
			return nil, fmt.Errorf("node %q detail request canceled or deadline exceeded: %w", nodeName, requestCtx.Err())
		case <-timer.C:
		}

		raw, requestErr = c.request(requestCtx, http.MethodGet, path+"?requestId="+url.QueryEscape(initial.RequestID), nil)
		if requestCtx.Err() != nil {
			return nil, fmt.Errorf("node %q detail request canceled or deadline exceeded: %w", nodeName, requestCtx.Err())
		}

		result, err := decodeNodeDetailResult(raw, requestErr, nodeName, initial.RequestID)
		if err != nil {
			return nil, err
		}

		if result.State == statusv1alpha1.NodeDetailComplete {
			return validateNodeDetails(result, time.Now())
		}

		if !result.Deadline.Equal(initial.Deadline) {
			return nil, fmt.Errorf("malformed node detail response: request deadline changed")
		}
	}
}

func decodeNodeDetailResult(raw []byte, requestErr error, nodeName, requestID string) (statusv1alpha1.NodeDetailResult, error) {
	var result statusv1alpha1.NodeDetailResult

	decodeErr := json.Unmarshal(raw, &result)
	if requestErr != nil && (decodeErr != nil || result.State == "") {
		return result, fmt.Errorf("node %q detail API unavailable or unsupported: %w", nodeName, requestErr)
	}

	if decodeErr != nil {
		return result, fmt.Errorf("malformed node detail response: %w", decodeErr)
	}

	if result.NodeName != nodeName || (requestID != "" && result.RequestID != requestID) {
		return result, fmt.Errorf("malformed node detail response: node or request identity mismatch")
	}

	switch result.State {
	case statusv1alpha1.NodeDetailComplete, statusv1alpha1.NodeDetailPending:
		if requestErr != nil {
			return result, fmt.Errorf("node detail request failed: %w", requestErr)
		}

		if result.State == statusv1alpha1.NodeDetailPending &&
			(result.RequestID == "" || result.Deadline.IsZero() || result.Details != nil) {
			return result, fmt.Errorf("malformed pending node detail response")
		}

		return result, nil
	case statusv1alpha1.NodeDetailExpired, statusv1alpha1.NodeDetailUnavailable, statusv1alpha1.NodeDetailRetryable:
		return result, fmt.Errorf("node %q details %s: %s", nodeName, result.State, result.Error)
	default:
		return result, fmt.Errorf("malformed or unsupported node detail state %q: %s", result.State, result.Error)
	}
}

func validateNodeDetails(result statusv1alpha1.NodeDetailResult, now time.Time) (*statusv1alpha1.NodeStatusResponse, error) {
	details := result.Details
	if result.RequestID == "" || details == nil || details.Status == nil ||
		details.NodeName != result.NodeName || details.RequestID != result.RequestID ||
		details.Status.NodeInfo.Name != result.NodeName || details.CollectedAt.IsZero() ||
		details.ReceivedAt.IsZero() || !details.ExpiresAt.After(details.ReceivedAt) || result.Error != "" {
		return nil, fmt.Errorf("malformed completed node detail response")
	}

	if !details.ExpiresAt.After(now) {
		return nil, fmt.Errorf("node %q details expired; request them again", result.NodeName)
	}

	if details.Status.FetchError != "" {
		return nil, fmt.Errorf("node %q detail collection failed: %s", result.NodeName, details.Status.FetchError)
	}

	return details.Status, nil
}

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
