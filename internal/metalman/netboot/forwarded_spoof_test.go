// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

// TestHTTPServerIgnoresForwardedHeaders verifies that a peer that is not a
// Machine cannot impersonate one by sending forwarding headers.
func TestHTTPServerIgnoresForwardedHeaders(t *testing.T) {
	cache := setupOCICache(t, "ghcr.io/test/image:v1", "spoof123", map[string][]byte{
		"grub/grub.cfg.tmpl": []byte("hostname={{ .Machine.Name }}"),
	})

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "victim"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:21", IPv4: "10.0.7.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(node).
		WithStatusSubresource(&v1alpha3.Machine{}).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).
		Build()

	newServer := func() (*HTTPServer, *recordingStatusRecorder) {
		recorder := &recordingStatusRecorder{}

		return &HTTPServer{
			Client: fc,
			FileResolver: FileResolver{
				Cache:             cache,
				Reader:            fc,
				Cluster:           &StaticClusterInfo{},
				DefaultNetbootRef: "ghcr.io/test/image:v1",
			},
			StatusRecorder: recorder,
		}, recorder
	}

	spoof := func(method, path, body string) *http.Request {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.RemoteAddr = "10.99.99.99:4321"
		r.Header.Set("X-Forwarded-For", "10.0.7.10")
		r.Header.Set("X-Real-IP", "10.0.7.10")
		r.Header.Set("Forwarded", "for=10.0.7.10")

		return r
	}

	t.Run("handleFile", func(t *testing.T) {
		srv, _ := newServer()
		w := httptest.NewRecorder()
		srv.handleFile(w, spoof(http.MethodGet, "/grub/grub.cfg", ""))
		require.Equal(t, http.StatusNotFound, w.Code)
		require.NotContains(t, w.Body.String(), "victim")

		// The real peer address still resolves the Machine.
		r := httptest.NewRequest(http.MethodGet, "/grub/grub.cfg", nil)
		r.RemoteAddr = "10.0.7.10:4321"
		w = httptest.NewRecorder()
		srv.handleFile(w, r)
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())
		require.Equal(t, "hostname=victim", w.Body.String())
	})

	t.Run("cloud-init log", func(t *testing.T) {
		srv, recorder := newServer()
		w := httptest.NewRecorder()
		srv.handleCloudInitLog(w, spoof(http.MethodPost, "/cloudinit/log", `{"name":"modules-final","event_type":"finish","result":"SUCCESS"}`))
		require.Equal(t, http.StatusOK, w.Code)
		require.Empty(t, recorder.conditionEvents())
		require.Empty(t, recorder.cloudInitDoneEvents())
	})

	t.Run("disable PXE", func(t *testing.T) {
		srv, recorder := newServer()
		w := httptest.NewRecorder()
		srv.handleDisablePXE(w, spoof(http.MethodPost, "/pxe/disable", ""))
		require.NotEqual(t, http.StatusOK, w.Code)
		require.Empty(t, recorder.pxeDisabledEvents())
		require.Empty(t, recorder.bootImageWrittenEvents())
	})
}
