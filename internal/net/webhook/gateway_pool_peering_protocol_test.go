// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/runtime"

	unboundednetv1alpha1 "github.com/Azure/unbounded/api/net/v1alpha1"
)

func TestValidateGatewayPoolPeeringTunnelProtocol(t *testing.T) {
	for _, operation := range []admissionv1.Operation{admissionv1.Create, admissionv1.Update} {
		for _, tt := range []struct {
			protocol *unboundednetv1alpha1.TunnelProtocol
			name     string
			allowed  bool
		}{
			{name: "unset", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolWireGuard), name: "WireGuard", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolIPIP), name: "IPIP", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolGENEVE), name: "GENEVE", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolVXLAN), name: "VXLAN", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolNone), name: "None", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocolAuto), name: "Auto", allowed: true},
			{protocol: new(unboundednetv1alpha1.TunnelProtocol("")), name: "empty"},
			{protocol: new(unboundednetv1alpha1.TunnelProtocol("wireguard")), name: "wrong case"},
			{protocol: new(unboundednetv1alpha1.TunnelProtocol("GRE")), name: "unsupported"},
		} {
			t.Run(string(operation)+"/"+tt.name, func(t *testing.T) {
				peering := unboundednetv1alpha1.GatewayPoolPeering{
					Spec: unboundednetv1alpha1.GatewayPoolPeeringSpec{
						GatewayPools:   []string{"local", "remote"},
						TunnelProtocol: tt.protocol,
					},
				}

				raw, err := json.Marshal(peering)
				if err != nil {
					t.Fatal(err)
				}

				validator := &Validator{}

				response := validator.validateGatewayPoolPeering(context.Background(), &admissionv1.AdmissionRequest{
					Operation: operation,
					Object:    runtime.RawExtension{Raw: raw},
				})
				if response.Allowed != tt.allowed {
					t.Fatalf("Allowed=%t, want %t; result=%v", response.Allowed, tt.allowed, response.Result)
				}

				if !tt.allowed && (response.Result == nil || !strings.Contains(response.Result.Message, "spec.tunnelProtocol")) {
					t.Fatalf("expected tunnelProtocol validation error, got %v", response.Result)
				}
			})
		}
	}
}
