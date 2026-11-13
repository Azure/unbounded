// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package machineops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/redfish"
)

type standardMediaTestClient struct {
	*redfish.Client
	calls *atomic.Int32
	err   error
}

func (c standardMediaTestClient) ForMachine(context.Context, *v1alpha3.Machine) (PowerClient, error) {
	if c.calls != nil {
		c.calls.Add(1)
	}

	return c.Client, c.err
}

func TestStandardVirtualMediaCapabilitiesFailBeforePower(t *testing.T) {
	for _, tc := range []struct {
		name       string
		allowed    []string
		parameters []map[string]any
		message    string
	}{
		{
			name: "cd not allowed", allowed: []string{"Pxe"},
			message: "neither boot override target UsbCd nor Cd is allowed",
		},
		{
			name: "required parameter", allowed: []string{"Cd"},
			parameters: []map[string]any{{"Name": "UserName", "Required": true}},
			message:    "requires unsupported parameter UserName",
		},
		{
			name: "protocol", allowed: []string{"Cd"},
			parameters: []map[string]any{{"Name": "TransferProtocolType", "AllowableValues": []string{"HTTPS"}}},
			message:    "does not allow transfer protocol HTTP",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mutations atomic.Int32

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path == "/redfish/v1/SessionService/Sessions" {
					http.NotFound(w, req)
					return
				}

				if req.Method != http.MethodGet {
					mutations.Add(1)
				}

				var body any

				switch req.URL.Path {
				case "/redfish/v1/Systems/1":
					body = map[string]any{
						"PowerState":   "On",
						"Boot":         map[string]any{"BootSourceOverrideTarget@Redfish.AllowableValues": tc.allowed},
						"VirtualMedia": map[string]string{"@odata.id": "/redfish/v1/Systems/1/VirtualMedia"},
					}
				case "/redfish/v1/Systems/1/VirtualMedia":
					body = map[string]any{"Members": []map[string]string{{"@odata.id": "/redfish/v1/Systems/1/VirtualMedia/1"}}}
				case "/redfish/v1/Systems/1/VirtualMedia/1":
					body = map[string]any{
						"MediaTypes": []string{"CD"}, "Inserted": false,
						"Actions": map[string]any{"#VirtualMedia.InsertMedia": map[string]string{
							"target":              "/redfish/v1/Systems/1/VirtualMedia/1/Actions/VirtualMedia.InsertMedia",
							"@Redfish.ActionInfo": "/redfish/v1/Systems/1/VirtualMedia/1/InsertMediaActionInfo",
						}},
					}
				case "/redfish/v1/Systems/1/VirtualMedia/1/InsertMediaActionInfo":
					body = map[string]any{"Parameters": tc.parameters}
				default:
					http.NotFound(w, req)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			t.Cleanup(srv.Close)
			bmc, err := redfish.Dial(t.Context(), srv.URL, "", "admin", "secret", "1")
			require.NoError(t, err)
			t.Cleanup(bmc.Close)

			r, d, _, op := mediaFixture(t)
			r.PowerClients = standardMediaTestClient{Client: bmc}
			mediaStep(t, r, op.Name)
			current := mediaStep(t, r, op.Name)
			require.Equal(t, "Staging", current.Status.Targets[0].BootMedia.Phase)
			current = mediaStep(t, r, op.Name)
			require.Equal(t, v1alpha3.OperationPhaseFailed, current.Status.Phase)
			require.Contains(t, current.Status.Targets[0].Message, reasonUnsupportedTarget)
			require.Contains(t, current.Status.Targets[0].Message, tc.message)
			require.Equal(t, []string{"prepare"}, d.calls)
			require.Zero(t, mutations.Load())

			for range 4 {
				current = mediaStep(t, r, op.Name)
			}

			require.Empty(t, current.Finalizers)
			require.True(t, current.Status.Targets[0].BootMedia.Released)
			require.Equal(t, []string{"prepare", "release"}, d.calls)
			require.Zero(t, mutations.Load())
		})
	}
}
