// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMediaAttestationFrozenLease(t *testing.T) {
	for _, result := range []string{"SUCCESS", "FAIL"} {
		t.Run(result, func(t *testing.T) {
			h, m, _ := firstBootFixture(t)
			r := httptest.NewRequest(http.MethodPost, "/attest", nil)
			r.RemoteAddr = "192.0.2.20:4321"
			r.Header.Set("X-Forwarded-For", "192.0.2.30")

			_, err := h.LookupNodeByIP(t.Context(), "192.0.2.20")
			require.Error(t, err)
			node, err := h.Media.LookupAttestationMachine(r)
			require.NoError(t, err)
			require.Equal(t, m.UID, node.UID)
			require.Equal(t, m.Spec, node.Spec)

			body := `{"name":"modules-final","event_type":"finish","result":"` + result + `"}`
			require.Equal(t, http.StatusOK, firstBootRequest(h, "/cloudinit/log", "192.0.2.20", body).Code)
			node, err = h.Media.LookupAttestationMachine(r)
			require.Error(t, err)
			require.Nil(t, node)
		})
	}
}

func TestMediaAttestationAuthorization(t *testing.T) {
	for _, scenario := range []string{
		"forged frozen IP", "forged live IP", "live IP", "changed protocol forged IP", "claimed lease",
		"uid mismatch", "site changed", "foreign media", "ambiguous", "not written", "no reboot", "abandoned", "invalid peer",
	} {
		t.Run(scenario, func(t *testing.T) {
			h, m, op := firstBootFixture(t)
			r := httptest.NewRequest(http.MethodPost, "/attest", nil)
			r.RemoteAddr = "192.0.2.20:4321"

			switch scenario {
			case "forged frozen IP":
				r.RemoteAddr = "192.0.2.30:4321"
				r.Header.Set("X-Forwarded-For", "192.0.2.20")
			case "forged live IP", "changed protocol forged IP":
				r.RemoteAddr = "192.0.2.30:4321"
				r.Header.Set("X-Forwarded-For", "192.0.2.99")

				if scenario == "changed protocol forged IP" {
					m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
				}
			case "live IP":
				r.RemoteAddr = "192.0.2.99:4321"
			case "claimed lease":
				other := m.DeepCopy()
				other.Name, other.UID, other.ResourceVersion = "other", "other-uid", ""
				other.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
				other.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
				require.NoError(t, h.Media.Client.Create(t.Context(), other))

				r.RemoteAddr = "192.0.2.30:4321"
				r.Header.Set("X-Forwarded-For", "192.0.2.20")
			case "uid mismatch":
				m.UID = "replacement-uid"
				m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
				m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
			case "site changed":
				m.Labels["unbounded-cloud.io/site"] = "site-b"
			case "foreign media":
				h.Media.Site = "site-b"
			case "ambiguous":
				other := op.DeepCopy()
				other.Name, other.UID, other.ResourceVersion = "duplicate", "duplicate-uid", ""
				require.NoError(t, h.Media.Client.Create(t.Context(), other))
			case "not written":
				op.Status.Targets[0].BootMedia.ImageWritten = false
			case "no reboot":
				op.Status.Targets[0].BootMedia.RebootRequestedAt = nil
			case "abandoned":
				op.Status.Targets[0].BootMedia.Abandoned = true
			case "invalid peer":
				r.RemoteAddr = "invalid"
			}

			require.NoError(t, h.Media.Client.Update(t.Context(), m))
			require.NoError(t, h.Media.Client.Status().Update(t.Context(), op))
			node, err := h.Media.LookupAttestationMachine(r)
			require.Error(t, err)
			require.Nil(t, node)
		})
	}
}

func TestMediaAttestationLegacyLookup(t *testing.T) {
	for _, protocol := range []string{v1alpha3.PXEBootProtocolPXE, v1alpha3.PXEBootProtocolHTTP} {
		for _, forwarded := range []bool{false, true} {
			t.Run(protocol+"/"+map[bool]string{false: "direct", true: "forwarded"}[forwarded], func(t *testing.T) {
				h, m, op := firstBootFixture(t)
				require.NoError(t, h.Media.Client.Delete(t.Context(), op))

				m.Spec.Netboot().BootProtocol = protocol
				require.NoError(t, h.Media.Client.Update(t.Context(), m))

				r := httptest.NewRequest(http.MethodPost, "/attest", nil)

				r.RemoteAddr = "192.0.2.99:4321"
				if forwarded {
					r.RemoteAddr = "192.0.2.30:4321"
					r.Header.Set("X-Forwarded-For", "192.0.2.99, 192.0.2.31")
				}

				node, err := h.Media.LookupAttestationMachine(r)
				require.NoError(t, err)
				require.Equal(t, client.ObjectKeyFromObject(m), client.ObjectKeyFromObject(node))
				require.Equal(t, m.Spec, node.Spec)
			})
		}
	}
}
