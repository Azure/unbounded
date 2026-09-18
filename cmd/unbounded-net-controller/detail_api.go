// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/Azure/unbounded/internal/net/authn"
	statusv1alpha1 "github.com/Azure/unbounded/internal/net/status/v1alpha1"
	webhookpkg "github.com/Azure/unbounded/internal/net/webhook"
)

func registerNodeDetailHandlers(mux *http.ServeMux, health *healthState, requireAuth bool, webhookServer *webhookpkg.Server, authorizer *dashboardAuthorizer, issuer *authn.TokenIssuer) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if !authorizeDashboardOrAggregated(requireAuth, issuer, authorizer, webhookServer, r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)

			return
		}

		nodeName := r.PathValue("name")
		manager := health.getDetailRequests()

		if !health.isLeader.Load() || manager == nil {
			writeNodeDetailResult(w, http.StatusServiceUnavailable,
				detailRequestFailure(nodeName, r.URL.Query().Get("requestId"), statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable"))

			return
		}

		var result statusv1alpha1.NodeDetailResult

		switch r.Method {
		case http.MethodPost:
			var input *struct {
				ForceRefresh bool `json:"forceRefresh"`
			}

			r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
			decoder := json.NewDecoder(r.Body)
			decoder.DisallowUnknownFields()

			err := decoder.Decode(&input)
			if err == nil {
				var extra any

				err = decoder.Decode(&extra)
				if errors.Is(err, io.EOF) && input != nil {
					err = nil
				} else if err == nil || input == nil {
					err = errors.New("expected one JSON object")
				}
			}

			if err != nil {
				code := http.StatusBadRequest

				var tooLarge *http.MaxBytesError
				if errors.As(err, &tooLarge) {
					code = http.StatusRequestEntityTooLarge
				}

				writeNodeDetailResult(w, code, detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailUnavailable, err.Error()))

				return
			}

			result = manager.Request(nodeName, input.ForceRefresh)
		case http.MethodGet:
			requestID := r.URL.Query().Get("requestId")
			if requestID == "" {
				writeNodeDetailResult(w, http.StatusBadRequest,
					detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailUnavailable, "requestId is required"))

				return
			}

			result = manager.Result(nodeName, requestID)
		default:
			w.Header().Set("Allow", "GET, POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

			return
		}

		writeNodeDetailResult(w, nodeDetailHTTPStatus(result.State), result)
	}
	mux.HandleFunc("/status/node/{name}/details", handler)

	if health.registerAggregatedAPIServer {
		mux.HandleFunc("/apis/status.net.unbounded-cloud.io/v1alpha1/nodes/{name}/details", func(w http.ResponseWriter, r *http.Request) {
			if !webhookServer.IsTrustedAggregatedRequest(r) {
				http.Error(w, "Forbidden", http.StatusForbidden)

				return
			}

			handler(w, r)
		})
	}
}

func nodeDetailHTTPStatus(state statusv1alpha1.NodeDetailState) int {
	switch state {
	case statusv1alpha1.NodeDetailPending:
		return http.StatusAccepted
	case statusv1alpha1.NodeDetailComplete:
		return http.StatusOK
	case statusv1alpha1.NodeDetailExpired:
		return http.StatusGone
	case statusv1alpha1.NodeDetailUnavailable:
		return http.StatusNotFound
	default:
		return http.StatusServiceUnavailable
	}
}

func serveLegacyNodeDetails(health *healthState, w http.ResponseWriter, r *http.Request, nodeName string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)

		return
	}

	manager := health.getDetailRequests()
	if manager == nil {
		writeNodeDetailResult(w, http.StatusServiceUnavailable,
			detailRequestFailure(nodeName, "", statusv1alpha1.NodeDetailRetryable, "detail request leader is unavailable"))

		return
	}

	result := manager.Request(nodeName, r.URL.Query().Get("live") == "true")
	if result.State == statusv1alpha1.NodeDetailPending {
		result = manager.Wait(r.Context(), nodeName, result.RequestID)
	}

	if result.State != statusv1alpha1.NodeDetailComplete || result.Details == nil {
		writeNodeDetailResult(w, nodeDetailHTTPStatus(result.State), result)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if err := json.NewEncoder(w).Encode(result.Details.Status); err != nil {
		klog.V(4).Infof("legacy node detail encode failed: %v", err)
	}
}

func writeNodeDetailResult(w http.ResponseWriter, code int, result statusv1alpha1.NodeDetailResult) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)

	if err := json.NewEncoder(w).Encode(result); err != nil {
		klog.V(4).Infof("node detail response encode failed: %v", err)
	}
}
