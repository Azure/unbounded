// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/bootmedia"
	"github.com/Azure/unbounded/internal/metalman/indexing"
)

func firstBootFixture(t *testing.T) (*HTTPServer, *v1alpha3.Machine, *v1alpha3.MachineOperation) {
	t.Helper()
	p, m, op := mediaFixture(t, "amd64")
	b := op.Status.Targets[0].BootMedia
	now := metav1.Now()
	b.ImageWritten, b.Cleaned, b.Released = true, true, true
	b.RebootRequestedAt = &now
	b.Netboot.CloudInit = &v1alpha3.CloudInitSpec{UserDataConfigMapRef: &v1alpha3.ConfigMapKeySelector{Name: "frozen-user-data", Namespace: "default"}}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "frozen-user-data", Namespace: "default"}, Data: map[string]string{"user-data": "#cloud-config\nhostname: frozen-host\n"}}
	op.Status.Phase = v1alpha3.OperationPhaseFailed
	m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.99"
	m.Spec.Netboot().DHCPLeases[0].Gateway = "192.0.2.254"
	m.Spec.Netboot().NetbootImage = "test/changed:latest"

	op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: "other-host"})
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(op).WithObjects(m, op, cm).
		WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
	p.Client, p.Resolver.Reader = fc, fc
	p.Resolver.Cluster = &StaticClusterInfo{}

	files := map[string][]byte{}
	for _, name := range []string{"meta-data", "vendor-data", "network-config"} {
		files["cloud-init/"+name+".tmpl"] = []byte(`{{ .Machine.Name }} {{ .BootLease.IPv4 }} {{ .BootLease.Gateway }}`)
	}

	require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), files))

	return &HTTPServer{Media: p, FileResolver: *p.Resolver, StatusRecorder: &recordingStatusRecorder{}}, m, op
}

func firstBootRequest(h *HTTPServer, path, ip, body string, forwarded ...string) *httptest.ResponseRecorder {
	method := http.MethodGet
	if body != "" {
		method = http.MethodPost
	}

	r := httptest.NewRequest(method, path, strings.NewReader(body))

	r.RemoteAddr = ip + ":4321"
	if len(forwarded) > 0 {
		r.Header.Set("X-Forwarded-For", forwarded[0])
	}

	w := httptest.NewRecorder()

	mux := http.NewServeMux()
	mux.HandleFunc("POST /cloudinit/log", h.handleCloudInitLog)
	mux.HandleFunc("POST /unbounded-agent/install-log", h.handleInstallLog)
	mux.HandleFunc("GET /pxe/disable", h.handleDisablePXE)
	mux.HandleFunc("POST /pxe/disable", h.handleDisablePXE)
	mux.HandleFunc("GET /", h.handleFile)
	normalizePath(mux).ServeHTTP(w, r)

	return w
}

func TestMediaFirstBootFrozenLeaseAndReports(t *testing.T) {
	for _, result := range []string{"SUCCESS", "FAIL"} {
		t.Run(result, func(t *testing.T) {
			h, m, op := firstBootFixture(t)
			for _, name := range []string{"meta-data", "vendor-data", "network-config", "user-data"} {
				w := firstBootRequest(h, "/cloud-init/"+name, "192.0.2.20", "")
				require.Equal(t, http.StatusOK, w.Code, w.Body.String())

				if name == "user-data" {
					require.Equal(t, "#cloud-config\nhostname: frozen-host\n", w.Body.String())
				} else {
					require.Equal(t, "host 192.0.2.20 192.0.2.1", w.Body.String())
				}
			}
			// First-boot access does not restore installer artifact access.
			require.Equal(t, http.StatusNotFound, firstBootRequest(h, "/vmlinuz", "192.0.2.20", "").Code)

			next := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name}}
			require.NoError(t, h.Media.Client.Create(t.Context(), next))
			require.Equal(t, http.StatusOK, firstBootRequest(h, "/unbounded-agent/install-log", "192.0.2.20", "install output").Code)

			for _, body := range []string{`{"name":"init-local","event_type":"start"}`, `{"name":"modules-final","event_type":"finish","result":"` + result + `"}`} {
				require.Equal(t, http.StatusOK, firstBootRequest(h, "/cloudinit/log", "192.0.2.20", body).Code)
				require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
				require.NotNil(t, apimeta.FindStatusCondition(op.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
			}

			cond := apimeta.FindStatusCondition(op.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
			if result == "SUCCESS" {
				require.Equal(t, metav1.ConditionTrue, cond.Status)
			} else {
				require.Equal(t, "Failed", cond.Reason)
			}

			require.Empty(t, op.Status.Targets[1].Conditions)
			require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(next), next))
			require.Empty(t, next.Status.Conditions)
			require.Empty(t, h.StatusRecorder.(*recordingStatusRecorder).conditionEvents())
			require.Equal(t, http.StatusNotFound, firstBootRequest(h, "/cloud-init/meta-data", "192.0.2.20", "").Code)
			require.Equal(t, http.StatusOK, firstBootRequest(h, "/cloudinit/log", "192.0.2.20", `{"name":"init-local","event_type":"start"}`).Code)
			require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			require.Equal(t, *cond, *apimeta.FindStatusCondition(op.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
		})
	}
}

func TestMediaFirstBootReportStatusPatch(t *testing.T) {
	for _, tt := range []struct {
		name        string
		eventType   string
		patchErr    error
		wantCode    int
		wantRetry   string
		wantPatches int
	}{
		{name: "success", eventType: "finish", wantCode: http.StatusOK, wantPatches: 1},
		{name: "patch failure", eventType: "finish", patchErr: fmt.Errorf("status patch unavailable"), wantCode: http.StatusServiceUnavailable, wantRetry: "5", wantPatches: 1},
		{name: "no condition update", eventType: "unknown", wantCode: http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h, _, op := firstBootFixture(t)
			patches := 0
			h.Media.Client = interceptor.NewClient(h.Media.Client.(client.WithWatch), interceptor.Funcs{
				SubResourcePatch: func(ctx context.Context, c client.Client, subResource string, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
					require.Equal(t, "status", subResource)
					require.Equal(t, client.ObjectKeyFromObject(op), client.ObjectKeyFromObject(obj))

					patches++

					if tt.patchErr != nil {
						return tt.patchErr
					}

					return c.SubResource(subResource).Patch(ctx, obj, patch, opts...)
				},
			})

			body := `{"name":"modules-final","event_type":"` + tt.eventType + `","result":"SUCCESS"}`
			w := firstBootRequest(h, "/cloudinit/log", "192.0.2.20", body)
			require.Equal(t, tt.wantCode, w.Code, w.Body.String())
			require.Equal(t, tt.wantRetry, w.Header().Get("Retry-After"))
			require.Equal(t, tt.wantPatches, patches)
			require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))

			cond := apimeta.FindStatusCondition(op.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone)
			if tt.patchErr == nil && tt.wantPatches > 0 {
				require.NotNil(t, cond)
				require.Equal(t, metav1.ConditionTrue, cond.Status)
				require.Equal(t, "Succeeded", cond.Reason)
			} else {
				require.Nil(t, cond)
			}

			require.Empty(t, op.Status.Targets[1].Conditions)
			require.Empty(t, h.StatusRecorder.(*recordingStatusRecorder).conditionEvents())
		})
	}
}

func TestMediaFirstBootAuthorization(t *testing.T) {
	for _, scenario := range []string{"uid mismatch", "uid mismatch live lease", "site changed", "abandoned", "not written", "no reboot", "ambiguous", "released successor"} {
		t.Run(scenario, func(t *testing.T) {
			h, m, op := firstBootFixture(t)

			switch scenario {
			case "site changed":
				m.Labels["unbounded-cloud.io/site"] = "site-b"
				require.NoError(t, h.Media.Client.Update(t.Context(), m))
			case "uid mismatch", "uid mismatch live lease":
				m.UID = "replacement-uid"
				if scenario == "uid mismatch live lease" {
					m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
				}

				require.NoError(t, h.Media.Client.Update(t.Context(), m))
			case "abandoned":
				op.Status.Targets[0].BootMedia.Abandoned = true
			case "not written":
				op.Status.Targets[0].BootMedia.ImageWritten = false
			case "no reboot":
				op.Status.Targets[0].BootMedia.RebootRequestedAt = nil
			case "ambiguous":
				other := op.DeepCopy()
				other.Name, other.UID, other.ResourceVersion = "duplicate", "duplicate-uid", ""
				require.NoError(t, h.Media.Client.Create(t.Context(), other))
			case "released successor":
				apimeta.SetStatusCondition(&op.Status.Targets[0].Conditions, metav1.Condition{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue, Reason: "Succeeded"})

				m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
				m.Spec.Netboot().NetbootImage = op.Status.Targets[0].BootMedia.Netboot.NetbootImage
				m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolHTTP
				require.NoError(t, h.Media.Client.Update(t.Context(), m))
				next := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name}}
				require.NoError(t, h.Media.Client.Create(t.Context(), next))
			}

			require.NoError(t, h.Media.Client.Status().Update(t.Context(), op))

			w := firstBootRequest(h, "/cloud-init/network-config", "192.0.2.20", "")
			if scenario == "released successor" {
				require.Equal(t, http.StatusOK, w.Code)
				require.Equal(t, "host 192.0.2.20 192.0.2.254", w.Body.String())
			} else {
				require.Equal(t, http.StatusNotFound, w.Code)
			}

			report := firstBootRequest(h, "/cloudinit/log", "192.0.2.20", `{"name":"modules-final","event_type":"finish","result":"SUCCESS"}`)
			if scenario == "released successor" || scenario == "abandoned" || scenario == "not written" {
				require.Equal(t, http.StatusOK, report.Code)
			} else {
				require.Equal(t, http.StatusNotFound, report.Code)
			}

			require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))

			if scenario == "released successor" {
				require.Len(t, h.StatusRecorder.(*recordingStatusRecorder).conditionEvents(), 1)
			} else {
				require.Empty(t, op.Status.Targets[0].Conditions)
				require.Empty(t, h.StatusRecorder.(*recordingStatusRecorder).conditionEvents())
			}
		})
	}
}

func TestMediaFirstBootIgnoresForwardedLease(t *testing.T) {
	h, _, _ := firstBootFixture(t)
	r := httptest.NewRequest(http.MethodGet, "/cloud-init/meta-data", nil)
	r.RemoteAddr = "192.0.2.30:4321"
	r.Header.Set("X-Forwarded-For", "192.0.2.20")

	w := httptest.NewRecorder()
	h.handleFile(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)

	r = httptest.NewRequest(http.MethodPost, "/cloudinit/log", strings.NewReader(`{"name":"modules-final","event_type":"finish","result":"SUCCESS"}`))
	r.RemoteAddr = "192.0.2.30:4321"
	r.Header.Set("X-Forwarded-For", "192.0.2.20")

	w = httptest.NewRecorder()
	h.handleCloudInitLog(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
	o, claimed, err := h.Media.firstBootOwner(t.Context(), "192.0.2.20")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Empty(t, o.op.Status.Targets[o.index].Conditions)
}

func TestMediaPathAliasesAuthorization(t *testing.T) {
	for _, scenario := range []string{"direct owner", "forged IP", "changed protocol", "changed protocol forged IP"} {
		t.Run(scenario, func(t *testing.T) {
			h, m, op := firstBootFixture(t)
			b := op.Status.Targets[0].BootMedia
			m.Spec.Netboot().NetbootImage = b.Netboot.NetbootImage
			m.Spec.Netboot().CloudInit = b.Netboot.CloudInit.DeepCopy()

			m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
			if strings.HasPrefix(scenario, "changed protocol") {
				m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
			}

			require.NoError(t, h.Media.Client.Update(t.Context(), m))
			require.NoError(t, populateOCICache(h.Media.CacheDir, strings.Repeat("a", 64), map[string][]byte{
				"grub/grub.cfg.tmpl": []byte("unscoped install configuration"),
				"vmlinuz.tmpl":       []byte("unscoped kernel"),
			}))

			remote, forwarded := "192.0.2.20", ""

			forged := strings.Contains(scenario, "forged")
			if forged {
				remote, forwarded = "192.0.2.30", "192.0.2.20"
			}

			for _, endpoint := range []struct{ path, body string }{
				{path: "/grub/grub.cfg"},
				{path: "/vmlinuz"},
				{path: "/cloud-init/meta-data"},
				{path: "/cloud-init/user-data"},
				{path: "/cloud-init/vendor-data"},
				{path: "/cloud-init/network-config"},
				{path: "/cloudinit/log", body: `{"name":"init-local","event_type":"start"}`},
				{path: "/unbounded-agent/install-log", body: "install output"},
				{path: "/pxe/disable"},
				{path: "/pxe/disable", body: "disable"},
			} {
				firstBoot := strings.HasPrefix(endpoint.path, "/cloud") || endpoint.path == "/unbounded-agent/install-log"
				// These combinations use legacy PXE authorization even on the canonical path.
				if !firstBoot && (scenario == "changed protocol forged IP" || (endpoint.path == "/pxe/disable" && scenario == "changed protocol")) {
					continue
				}

				want := http.StatusNotFound
				if firstBoot && !forged {
					want = http.StatusOK
				}

				for _, alias := range requestPathAliases(endpoint.path) {
					t.Run(alias+"/"+endpoint.body, func(t *testing.T) {
						w := firstBootRequest(h, alias, remote, endpoint.body, forwarded)
						require.Equal(t, want, w.Code, w.Body.String())

						if want == http.StatusOK && strings.HasPrefix(endpoint.path, "/cloud-init/") {
							if endpoint.path == "/cloud-init/user-data" {
								require.Equal(t, "#cloud-config\nhostname: frozen-host\n", w.Body.String())
							} else {
								require.Equal(t, "host 192.0.2.20 192.0.2.1", w.Body.String())
							}
						}

						if forged {
							require.NoError(t, h.Media.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
							require.Empty(t, op.Status.Targets[0].Conditions)
						}
					})
				}
			}

			require.Empty(t, h.StatusRecorder.(*recordingStatusRecorder).conditionEvents())
		})
	}
}

func requestPathAliases(path string) []string {
	return []string{
		path,
		path + "/",
		path + "//",
		strings.ReplaceAll(path, "/", "//"),
		"/./" + strings.TrimPrefix(path, "/"),
		"/unused/.." + path,
		path + "/.",
		path + "/child/..",
		strings.ReplaceAll(path, "/", "/%2e/"),
		"/" + strings.ReplaceAll(strings.TrimPrefix(path, "/"), "/", "%2f"),
	}
}

func TestFirstBootForwardedAuthorization(t *testing.T) {
	for _, scenario := range []string{
		"forged live IP", "forged frozen IP", "direct live IP", "direct frozen IP",
		"direct owner ignores XFF", "changed protocol", "before handoff", "no retained owner",
		"completed media", "no provider", "PXE", "HTTP", "PXE without provider",
	} {
		t.Run(scenario, func(t *testing.T) {
			for _, endpoint := range []struct{ path, body string }{
				{path: "/cloud-init/meta-data"},
				{path: "/cloud-init/user-data"},
				{path: "/cloud-init/vendor-data"},
				{path: "/cloud-init/network-config"},
				{path: "/unbounded-agent/install-log", body: "install output"},
				{path: "/cloudinit/log", body: `{"name":"modules-final","event_type":"finish","result":"SUCCESS"}`},
			} {
				t.Run(endpoint.path, func(t *testing.T) {
					h, m, op := firstBootFixture(t)
					fc := h.Media.Client
					b := op.Status.Targets[0].BootMedia
					m.Spec.Netboot().NetbootImage = b.Netboot.NetbootImage
					m.Spec.Netboot().CloudInit = b.Netboot.CloudInit.DeepCopy()
					remote, forwarded := "192.0.2.30", "192.0.2.99"
					want := http.StatusNotFound
					mediaReport := false
					legacyReport := false

					switch scenario {
					case "forged live IP":
						m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.20"
						forwarded = "192.0.2.20"
					case "forged frozen IP":
						forwarded = "192.0.2.20"
					case "direct live IP":
						remote, forwarded = "192.0.2.99", ""
					case "direct frozen IP":
						remote, forwarded = "192.0.2.20", ""
						want, mediaReport = http.StatusOK, true
					case "direct owner ignores XFF":
						other := m.DeepCopy()
						other.Name, other.UID, other.ResourceVersion = "other", "other-uid", ""
						other.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
						other.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.40"
						require.NoError(t, fc.Create(t.Context(), other))

						remote, forwarded = "192.0.2.20", "192.0.2.40"
						want, mediaReport = http.StatusOK, true
					case "changed protocol":
						m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
					case "before handoff":
						m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
						b.ImageWritten, b.Released = false, false
						b.RebootRequestedAt = nil
					case "completed media":
						apimeta.SetStatusCondition(&op.Status.Targets[0].Conditions, metav1.Condition{Type: v1alpha3.MachineOperationConditionCloudInitDone, Status: metav1.ConditionTrue, Reason: "Succeeded"})
					case "no retained owner":
						op.Status.Targets[0].BootMedia = nil
					case "no provider":
						h.Media = nil
					case "PXE", "HTTP", "PXE without provider":
						op.Status.Targets[0].BootMedia = nil

						m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolPXE
						if scenario == "HTTP" {
							m.Spec.Netboot().BootProtocol = v1alpha3.PXEBootProtocolHTTP
						}

						if scenario == "PXE without provider" {
							h.Media = nil
						}

						want, legacyReport = http.StatusOK, true
					}

					require.NoError(t, fc.Update(t.Context(), m))
					require.NoError(t, fc.Status().Update(t.Context(), op))
					before := op.DeepCopy()
					w := firstBootRequest(h, endpoint.path, remote, endpoint.body, forwarded)
					require.Equal(t, want, w.Code, w.Body.String())
					require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(op), op))

					if mediaReport && endpoint.path == "/cloudinit/log" {
						require.True(t, apimeta.IsStatusConditionTrue(op.Status.Targets[0].Conditions, v1alpha3.MachineOperationConditionCloudInitDone))
						require.Empty(t, op.Status.Conditions)
						require.Empty(t, op.Status.Targets[1].Conditions)
					} else {
						require.Equal(t, before.Status, op.Status)
					}

					if want == http.StatusOK && endpoint.path == "/cloud-init/user-data" {
						require.Equal(t, "#cloud-config\nhostname: frozen-host\n", w.Body.String())
					}

					recorder := h.StatusRecorder.(*recordingStatusRecorder)
					if legacyReport && endpoint.path == "/cloudinit/log" {
						require.Len(t, recorder.conditionEvents(), 1)
						require.Equal(t, m.Name, recorder.conditionEvents()[0].machineName)
						require.Equal(t, []string{m.Name}, recorder.cloudInitDoneEvents())
					} else {
						require.Empty(t, recorder.conditionEvents())
						require.Empty(t, recorder.cloudInitDoneEvents())
					}
				})
			}
		})
	}
}

func TestMediaArtifactForwardedAuthorization(t *testing.T) {
	for _, protocol := range []string{v1alpha3.PXEBootProtocolPXE, v1alpha3.PXEBootProtocolHTTP} {
		for _, scenario := range []string{"retained", "cleanup pending", "active media", "unclaimed", "other site", "successor"} {
			t.Run(protocol+"/"+scenario, func(t *testing.T) {
				p, m, old := mediaFixture(t, "amd64")
				m.Spec.Netboot().BootProtocol = protocol
				m.Spec.Netboot().NetbootImage = "test/live@sha256:" + strings.Repeat("d", 64)
				old.Status.Phase = v1alpha3.OperationPhaseComplete
				media := old.Status.Targets[0].BootMedia
				media.Cleaned, media.Released = true, true
				want := http.StatusNotFound

				switch scenario {
				case "cleanup pending":
					media.Released = false
				case "active media":
					old.Status.Phase = ""
					media.Cleaned, media.Released = false, false
				case "unclaimed":
					m.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.40"
					want = http.StatusOK
				case "other site":
					media.Site = "site-b"
					want = http.StatusOK
				case "successor":
					want = http.StatusOK
				}

				fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(old).WithObjects(m, old).
					WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
				p.Client, p.Resolver.Reader = fc, fc

				if scenario == "successor" {
					next := &v1alpha3.MachineOperation{
						ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"},
						Spec: v1alpha3.MachineOperationSpec{
							OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name,
						},
						Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name}}},
					}
					require.NoError(t, fc.Create(t.Context(), next))
				}

				files := map[string][]byte{
					"vmlinuz": []byte("live kernel"), "initrd": []byte("live initrd"), "init.cpio": []byte("live init.cpio"),
					"grub.cfg": []byte("live grub config"), "grub/grub.cfg": []byte("live grub config"),
					"disk.img.gz": []byte("live disk"),
				}
				require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("d", 64), files))
				require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("b", 64), files))
				p.Resolver.Cache.SetDigest(m.Spec.Netboot().NetbootImage, "sha256:"+strings.Repeat("d", 64))
				p.Resolver.Cache.SetDigest(m.Spec.Netboot().Image, "sha256:"+strings.Repeat("b", 64))
				h := &HTTPServer{Media: p, FileResolver: *p.Resolver}

				for path, content := range files {
					t.Run(path, func(t *testing.T) {
						ip := m.Spec.Netboot().DHCPLeases[0].IPv4
						for _, forwarded := range []string{ip, " " + ip + ", 192.0.2.31"} {
							w := firstBootRequest(h, "/"+path, "192.0.2.30", "", forwarded)
							require.Equal(t, want, w.Code, w.Body.String())

							if want == http.StatusOK {
								require.Equal(t, string(content), w.Body.String())
							}
						}

						if scenario != "active media" {
							w := firstBootRequest(h, "/"+path, ip, "")
							require.Equal(t, want, w.Code, w.Body.String())
						}
					})
				}
			})
		}
	}
}

func TestMediaReleasedHistorySuccessorPrecedence(t *testing.T) {
	for _, protocol := range []string{v1alpha3.PXEBootProtocolPXE, v1alpha3.PXEBootProtocolHTTP} {
		t.Run(protocol, func(t *testing.T) {
			p, m, old := mediaFixture(t, "amd64")
			old.Status.Phase = v1alpha3.OperationPhaseComplete
			media := old.Status.Targets[0].BootMedia
			media.Cleaned, media.Released, media.ImageWritten = true, true, true
			m.Spec.Netboot().BootProtocol = protocol
			next := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "successor", UID: "successor-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace, MachineRef: m.Name}, Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name}}}}
			fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(old).WithObjects(m, old).
				WithIndex(&v1alpha3.Machine{}, indexing.IndexNodeByIP, indexing.IndexNodeByIPFunc).Build()
			p.Client, p.Resolver.Reader = fc, fc
			require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), map[string][]byte{"vmlinuz": []byte("successor kernel")}))
			h := &HTTPServer{Media: p, FileResolver: *p.Resolver}
			request := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodGet, "/vmlinuz", nil)
				r.RemoteAddr = "192.0.2.20:4321"
				w := httptest.NewRecorder()
				h.handleFile(w, r)

				return w
			}
			// Protocol edits alone cannot supersede a stale media install.
			require.Equal(t, http.StatusNotFound, request().Code)
			require.NoError(t, fc.Create(t.Context(), next))

			w := request()
			require.Equal(t, http.StatusOK, w.Code)
			require.Equal(t, "successor kernel", w.Body.String())
			// A current VirtualMedia Machine never falls through to PXE/HTTP.
			m.Spec.Netboot().BootProtocol = "VirtualMedia"
			require.NoError(t, fc.Update(t.Context(), m))
			require.Equal(t, http.StatusNotFound, request().Code)

			m.Spec.Netboot().BootProtocol = protocol
			require.NoError(t, fc.Update(t.Context(), m))
			// Cleanup still in progress takes precedence over the successor.
			require.NoError(t, fc.Get(t.Context(), client.ObjectKeyFromObject(old), old))
			old.Status.Targets[0].BootMedia.Released = false
			require.NoError(t, fc.Status().Update(t.Context(), old))
			require.Equal(t, http.StatusNotFound, request().Code)
			// An active media owner keeps its frozen execution input even if a
			// non-media operation and mutable protocol claim the same lease.
			old.Status.Phase = ""
			old.Status.Targets[0].BootMedia.Cleaned = false
			old.Status.Targets[0].BootMedia.ImageWritten = false
			require.NoError(t, fc.Status().Update(t.Context(), old))
			o, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
			require.NoError(t, err)
			require.True(t, claimed)
			require.Equal(t, old.Name, o.op.Name)
		})
	}
}

func mediaFixture(t *testing.T, arch string) (*MediaProvider, *v1alpha3.Machine, *v1alpha3.MachineOperation) {
	t.Helper()

	ref := "test/netboot@sha256:" + strings.Repeat("a", 64)

	shim, grub := "shimx64.efi", "grubx64.efi"
	if arch == "arm64" {
		shim, grub = "shimaa64.efi", "grubaa64.efi"
	}

	tmpl, err := os.ReadFile("../../../images/netboot/assets/grub.cfg.tmpl")
	require.NoError(t, err)
	cache := NewOCICache(t.TempDir())
	require.NoError(t, populateOCICacheForArchitecture(cache.CacheDir, strings.Repeat("a", 64), arch, map[string][]byte{
		"metadata.yaml": []byte("mediaFormat: " + mediaFormat + "\n"),
		shim:            []byte("unchanged shim " + arch), grub: []byte("unchanged grub " + arch), "grub/grub.cfg.tmpl": tmpl,
	}))
	cache.SetDigestForArchitecture(ref, arch, "sha256:"+strings.Repeat("a", 64))
	m := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "host", UID: "host-uid", Labels: map[string]string{"unbounded-cloud.io/site": "site-a"}}, Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
		Image: "test/disk@sha256:" + strings.Repeat("b", 64), NetbootImage: ref, Architecture: arch, BootProtocol: "VirtualMedia",
		DHCPLeases: []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}},
	}}}}
	op := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "replace", UID: "op-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace}, Status: v1alpha3.MachineOperationStatus{
		Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: m.Name, BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
			ID: strings.Repeat("c", 32), Site: "site-a", MachineUID: m.UID, Netboot: m.Spec.Netboot().DeepCopy(), Phase: "Preparing",
		}}},
	}}
	fc := fake.NewClientBuilder().WithScheme(newScheme(t)).WithStatusSubresource(op).WithObjects(m, op).Build()
	p := &MediaProvider{Site: "site-a", Resolver: &FileResolver{Cache: cache, Reader: fc, ServeURL: "http://192.0.2.2:8080"}, Client: fc, CacheDir: cache.CacheDir, TFTPServer: "192.0.2.2"}

	return p, m, op
}

func mockMediaRunner(t *testing.T, calls *[]string) func(context.Context, string, ...string) error {
	t.Helper()

	return func(_ context.Context, name string, args ...string) error {
		*calls = append(*calls, name+" "+strings.Join(args, " "))
		if name == "xorriso" {
			for i, arg := range args {
				if arg == "-o" {
					return os.WriteFile(args[i+1], []byte("immutable ISO"), 0o600)
				}
			}
		}

		return nil
	}
}

func TestMediaPrepareAndRestart(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			p, machine, op := mediaFixture(t, arch)

			var calls []string

			var config []byte

			run := mockMediaRunner(t, &calls)
			p.Run = func(ctx context.Context, name string, args ...string) error {
				if name == "mcopy" && args[len(args)-1] == "::/EFI/BOOT/grub.cfg" {
					var err error

					config, err = os.ReadFile(args[len(args)-2])
					require.NoError(t, err)
				}

				return run(ctx, name, args...)
			}
			id := op.Status.Targets[0].BootMedia.ID
			source, err := p.Prepare(t.Context(), machine, op, id)
			require.NoError(t, err)
			require.Equal(t, "ub-"+id+".iso", source.Name)
			require.Len(t, source.Name, 39)
			require.Equal(t, "192.0.2.2", source.Server)

			payload := strings.Join(calls, "\n")
			if arch == "amd64" {
				require.Contains(t, payload, "shimx64.efi ::/EFI/BOOT/BOOTX64.EFI")
				require.Contains(t, payload, "grubx64.efi ::/EFI/BOOT/grubx64.efi")
			} else {
				require.Contains(t, payload, "shimaa64.efi ::/EFI/BOOT/BOOTAA64.EFI")
				require.Contains(t, payload, "grubaa64.efi ::/EFI/BOOT/grubaa64.efi")
			}

			require.Contains(t, payload, "mkfs.vfat --invariant -F 16 -s 1 -n UBMEDIA")
			require.Contains(t, payload, "-append_partition 2 0xef ")
			require.Contains(t, payload, "-appended_part_as_gpt -e --interval:appended_partition_2:all:: -no-emul-boot")

			entries, err := os.ReadDir(filepath.Join(p.CacheDir, "bootmedia", id))
			require.NoError(t, err)
			require.Len(t, entries, 2)
			require.Equal(t, "bootstrap.iso", entries[0].Name())
			require.Equal(t, "identity", entries[1].Name())

			builds, err := os.ReadDir(filepath.Join(p.CacheDir, "bootmedia"))
			require.NoError(t, err)
			require.Len(t, builds, 1, "build intermediates must be removed")

			for _, wanted := range []string{"net_add_addr ubprobe efinet0 0.0.0.0", "net_add_addr ubprobe efinet63 0.0.0.0", "$net_ubprobe_mac", "02:11:22:33:44:55", "net_add_addr ubnet $ubnic 192.0.2.20", "net_del_route ubnet:local", "net_add_route ublocal 192.0.2.0/24 ubnet", "net_add_route ubdefault 0.0.0.0/0 gw 192.0.2.1", "configfile (http,192.0.2.2:8080)/bootmedia/" + id + "/grub.cfg"} {
				require.Contains(t, string(config), wanted)
			}

			for _, forbidden := range []string{"dhcp", "unbounded.image_url", "password", "token", "chainloader"} {
				require.NotContains(t, string(config), forbidden)
			}

			require.Contains(t, string(config), "for ubtry in 1 2 3 4 5 6 7 8 9 10; do")
			require.Contains(t, string(config), "sleep 6\ndone\nhalt\n")

			count := len(calls)
			restarted := &MediaProvider{Site: p.Site, Resolver: p.Resolver, Client: p.Client, CacheDir: p.CacheDir, TFTPServer: p.TFTPServer, Run: p.Run}
			f, err := restarted.OpenISO(t.Context(), source.Name)
			require.NoError(t, err)
			require.NoError(t, f.Close())
			require.Len(t, calls, count)
			// A rack caller has no host-IP lookup at all on the media path.
			server := &TFTPServer{Media: restarted}

			var dst strings.Builder
			require.NoError(t, server.readHandler(source.Name, &readerFrom{Writer: &dst}))
			require.Equal(t, "immutable ISO", dst.String())

			// The primer name serves the same ISO.
			var primer strings.Builder
			require.NoError(t, server.readHandler(bootmedia.PrimerName(source.Name), &readerFrom{Writer: &primer}))
			require.Equal(t, "immutable ISO", primer.String())

			for _, bad := range []string{"/" + source.Name, "../" + source.Name, "ub-unknown.iso", "ub-" + strings.Repeat("d", 32) + ".iso", "ub-" + strings.Repeat("d", 64) + ".iso", "uq-" + id + ".iso", "up-" + id + ".img"} {
				_, err := restarted.OpenISO(t.Context(), bad)
				require.Error(t, err)
			}

			require.NoError(t, p.Release(t.Context(), id))
			_, err = p.Prepare(t.Context(), machine, op, id)
			require.NoError(t, err)
			require.Greater(t, len(calls), count)
		})
	}
}

type readerFrom struct{ io.Writer }

func (w *readerFrom) ReadFrom(r io.Reader) (int64, error) { return io.Copy(w.Writer, r) }

func TestMediaBuildFailureCleanup(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")
	p.Run = func(context.Context, string, ...string) error { return fmt.Errorf("tool failed") }
	_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.ErrorContains(t, err, "tool failed")
	entries, err := os.ReadDir(filepath.Join(p.CacheDir, "bootmedia"))
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestMediaRejectInputs(t *testing.T) {
	for _, raw := range []string{"https://192.0.2.2", "http://host", "http://u:p@192.0.2.2", "http://192.0.2.2/?x", "http://192.0.2.2/#x", "http://192.0.2.2/a;halt", "http://192.0.2.2/%0a", "http://192.0.2.2:99999", "http://192.0.2.2/base"} {
		_, err := mediaServeURL(raw)
		require.Error(t, err, raw)
	}

	for _, mutate := range []func(*v1alpha3.PXESpec){
		func(s *v1alpha3.PXESpec) { s.DHCPLeases = nil },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].Gateway = "" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].Gateway = "198.51.100.1" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].IPv4 = "192.0.2.20;halt" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].MAC = "02:11:22:33:44:55\nhalt" },
		func(s *v1alpha3.PXESpec) { s.DHCPLeases[0].SubnetMask = "255.0.255.0" },
		func(s *v1alpha3.PXESpec) { s.TargetDisk = "/dev/sda;halt" },
	} {
		_, m, _ := mediaFixture(t, "amd64")
		mutate(m.Spec.Netboot())
		require.Error(t, validateMediaLease(m.Spec.Netboot()))
	}

	p, m, op := mediaFixture(t, "amd64")
	op.Status.Targets[0].BootMedia.Netboot.Image = "test/disk:mutable"
	m.Spec.Netboot().Image = "test/disk:mutable"
	require.NoError(t, p.Client.Update(t.Context(), m))
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.ErrorContains(t, err, "digest-pinned")
}

func TestMediaCapabilityLength(t *testing.T) {
	p, _, op := mediaFixture(t, "arm64")
	id := op.Status.Targets[0].BootMedia.ID
	_, err := p.callbackOwner(t.Context(), id)
	require.NoError(t, err)

	for _, invalid := range []string{"", id[:31], id + "c", strings.Repeat("d", 64), strings.ToUpper(id), strings.Repeat("g", 32)} {
		t.Run(fmt.Sprintf("%q", invalid), func(t *testing.T) {
			// Persist even an invalid ID so rejection cannot be due to no owner.
			op.Status.Targets[0].BootMedia.ID = invalid
			require.NoError(t, p.Client.Status().Update(t.Context(), op))
			_, err := p.callbackOwner(t.Context(), invalid)
			require.ErrorContains(t, err, "invalid media capability")
		})
	}
}

func setMediaPhase(t *testing.T, p *MediaProvider, op *v1alpha3.MachineOperation, phase string) {
	t.Helper()

	var current v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	current.Status.Targets[0].BootMedia.Phase = phase
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	*op = current
}

func mediaRequest(p *MediaProvider, id, method, action, ip string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/bootmedia/"+id+"/"+action, nil)
	r.RemoteAddr = ip + ":4321"
	r.Header.Set("X-Forwarded-For", "192.0.2.20")

	w := httptest.NewRecorder()
	p.ServeHTTP(w, r)

	return w
}

func TestMediaScopedCallbacks(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	op.Status.Targets = append(op.Status.Targets, v1alpha3.MachineOperationTargetStatus{MachineRef: "other-host", BootMedia: &v1alpha3.MachineOperationBootMediaStatus{ID: strings.Repeat("d", 32), Site: p.Site}})
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	id := op.Status.Targets[0].BootMedia.ID
	// Before power-on the host is not expected to boot, so it gets halt.
	require.Equal(t, "halt\n", mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Body.String())
	setMediaPhase(t, p, op, "Installing")

	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)

	var started v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &started))
	require.NotNil(t, started.Status.Targets[0].BootMedia.LoaderStartedAt)
	require.Nil(t, started.Status.Targets[1].BootMedia.LoaderStartedAt)
	require.Contains(t, w.Body.String(), "linux (http,192.0.2.2:8080)/vmlinuz")
	require.Contains(t, w.Body.String(), "initrd (http,192.0.2.2:8080)/initrd (http,192.0.2.2:8080)/init.cpio")
	require.Contains(t, w.Body.String(), "unbounded.virtual_media=1")
	require.Contains(t, w.Body.String(), "unbounded.media_callback=http://192.0.2.2:8080/bootmedia/"+id)
	require.NotContains(t, w.Body.String(), "chainloader")
	require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.99").Code)
	require.Equal(t, http.StatusMethodNotAllowed, mediaRequest(p, id, http.MethodGet, "complete", "192.0.2.20").Code)
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)

	for range 2 {
		require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.20").Code)
	}

	var current v1alpha3.MachineOperation
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.True(t, current.Status.Targets[0].BootMedia.ImageWritten)
	require.False(t, current.Status.Targets[1].BootMedia.ImageWritten)
	require.Empty(t, current.Status.Conditions)
	require.Equal(t, "halt\n", mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Body.String())
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)

	current.Status.Targets[0].BootMedia.Cleaned = true
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.NotNil(t, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	ack := current.Status.Targets[0].BootMedia.RebootRequestedAt.DeepCopy()

	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), &current))
	require.Equal(t, ack, current.Status.Targets[0].BootMedia.RebootRequestedAt)
	_, err := p.OpenISO(t.Context(), "ub-"+id+".iso")
	require.Error(t, err)

	current.Status.Phase = v1alpha3.OperationPhaseComplete
	require.NoError(t, p.Client.Status().Update(t.Context(), &current))
	require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
}

func TestMediaRejectPathAliasesAndArtifactDrift(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")

	id := op.Status.Targets[0].BootMedia.ID
	for _, path := range []string{"/bootmedia/" + id + "/../grub.cfg", "/bootmedia//" + id + "/grub.cfg", "/bootmedia/%63" + id[1:] + "/grub.cfg", "/bootmedia/" + id + "/grub.cfg/"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.RemoteAddr = "192.0.2.20:4321"
		w := httptest.NewRecorder()
		normalizePath(p).ServeHTTP(w, r)
		require.Equal(t, http.StatusNotFound, w.Code)
	}

	_, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, _ = p.artifactOwner(t.Context(), "192.0.2.21")
	require.False(t, claimed)

	m.Spec.Netboot().Image = "other/disk@sha256:" + strings.Repeat("e", 64)
	require.NoError(t, p.Client.Update(t.Context(), m))
	o, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
	require.NoError(t, err)
	require.True(t, claimed)
	require.Equal(t, op.Status.Targets[0].BootMedia.Netboot.Image, o.machine.Spec.Netboot().Image)
}

func TestMediaRevokedLeaseClaimBlocksReassignedIP(t *testing.T) {
	for _, released := range []bool{false, true} {
		t.Run(fmt.Sprintf("released=%t", released), func(t *testing.T) {
			p, _, op := mediaFixture(t, "amd64")
			// A failed operation for another Machine still claims the IP. Its
			// installer may be running, so its claim must not be ignored.
			stale := &v1alpha3.MachineOperation{ObjectMeta: metav1.ObjectMeta{Name: "stale", UID: "stale-uid"}, Spec: v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace}, Status: v1alpha3.MachineOperationStatus{
				Phase: v1alpha3.OperationPhaseFailed,
				Targets: []v1alpha3.MachineOperationTargetStatus{{MachineRef: "old-host", Phase: v1alpha3.OperationPhaseFailed, BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
					ID: strings.Repeat("d", 32), Site: "site-a", MachineUID: "old-host-uid", Netboot: op.Status.Targets[0].BootMedia.Netboot.DeepCopy(), Phase: "Installing",
					Cleaned: released, Released: released,
				}}},
			}}
			require.NoError(t, p.Client.Create(t.Context(), stale))
			require.NoError(t, p.Client.Status().Update(t.Context(), stale))

			o, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
			require.True(t, claimed)

			if released {
				require.NoError(t, err)
				require.Equal(t, op.Name, o.op.Name)

				return
			}

			require.ErrorContains(t, err, "ambiguous media lease")
		})
	}
}

func TestMediaCallbackPersistenceFailure(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	p.Client = interceptor.NewClient(p.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return fmt.Errorf("conflict")
	}})
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, op.Status.Targets[0].BootMedia.ID, http.MethodPost, "complete", "192.0.2.20").Code)
}

func TestMediaCallbacksOutliveInstallAuthorization(t *testing.T) {
	for _, state := range []string{"operation-failed", "target-failed", "operation-deleting", "machine-deleted", "machine-replaced"} {
		t.Run(state, func(t *testing.T) {
			p, m, op := mediaFixture(t, "amd64")
			id := op.Status.Targets[0].BootMedia.ID

			switch state {
			case "operation-failed":
				op.Status.Phase = v1alpha3.OperationPhaseFailed
			case "target-failed":
				op.Status.Targets[0].Phase = v1alpha3.OperationPhaseFailed
			case "operation-deleting":
				op.Finalizers = []string{"test.example/cleanup"}
				require.NoError(t, p.Client.Update(t.Context(), op))
				require.NoError(t, p.Client.Delete(t.Context(), op))
				require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			case "machine-deleted", "machine-replaced":
				require.NoError(t, p.Client.Delete(t.Context(), m))

				if state == "machine-replaced" {
					m.UID, m.ResourceVersion = "replacement", ""
					require.NoError(t, p.Client.Create(t.Context(), m))
				}
			}

			require.NoError(t, p.Client.Status().Update(t.Context(), op))
			require.Equal(t, "halt\n", mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20").Body.String())
			_, claimed, err := p.artifactOwner(t.Context(), "192.0.2.20")
			require.True(t, claimed)
			require.Error(t, err)
			require.Equal(t, http.StatusNotFound, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.21").Code)
			require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodPost, "complete", "192.0.2.20").Code)
			require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
			require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			op.Status.Targets[0].BootMedia.Cleaned = true
			require.NoError(t, p.Client.Status().Update(t.Context(), op))
			require.Equal(t, http.StatusOK, mediaRequest(p, id, http.MethodGet, "ready", "192.0.2.20").Code)
			require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
			require.NotNil(t, op.Status.Targets[0].BootMedia.RebootRequestedAt)
		})
	}
}

func TestMediaCallbackAbandonment(t *testing.T) {
	for _, abandoned := range []bool{false, true} {
		for _, action := range []string{"complete", "ready", "grub.cfg"} {
			t.Run(fmt.Sprintf("abandoned=%t/%s", abandoned, action), func(t *testing.T) {
				p, _, op := mediaFixture(t, "amd64")
				target := &op.Status.Targets[0]
				target.Phase = v1alpha3.OperationPhaseFailed
				b := target.BootMedia
				b.Abandoned = abandoned
				b.Cleaned, b.Released = true, true
				b.ImageWritten = action == "ready"

				require.NoError(t, p.Client.Status().Update(t.Context(), op))
				before := op.DeepCopy()

				method := http.MethodGet
				if action == "complete" {
					method = http.MethodPost
				}

				w := mediaRequest(p, b.ID, method, action, "192.0.2.20")
				require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))

				if abandoned {
					require.Equal(t, http.StatusNotFound, w.Code)
					require.Equal(t, before.Status, op.Status)

					return
				}

				require.Equal(t, http.StatusOK, w.Code)

				switch action {
				case "complete":
					require.True(t, op.Status.Targets[0].BootMedia.ImageWritten)
				case "ready":
					require.NotNil(t, op.Status.Targets[0].BootMedia.RebootRequestedAt)
				case "grub.cfg":
					require.Equal(t, "halt\n", w.Body.String())
					require.Equal(t, before.Status, op.Status)
				}
			})
		}
	}
}

func TestMediaReadyPersistenceFailure(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	op.Status.Targets[0].BootMedia.Cleaned = true
	op.Status.Targets[0].BootMedia.ImageWritten = true
	require.NoError(t, p.Client.Status().Update(t.Context(), op))
	p.Client = interceptor.NewClient(p.Client.(client.WithWatch), interceptor.Funcs{SubResourcePatch: func(context.Context, client.Client, string, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
		return fmt.Errorf("conflict")
	}})
	require.Equal(t, http.StatusServiceUnavailable, mediaRequest(p, op.Status.Targets[0].BootMedia.ID, http.MethodGet, "ready", "192.0.2.20").Code)
}

func TestMediaInstallerWaitsThroughLateCleanup(t *testing.T) {
	content, err := os.ReadFile("../../../images/netboot/assets/init")
	require.NoError(t, err)

	_, barrier, found := strings.Cut(string(content), "if [ \"$VIRTUAL_MEDIA\" = 1 ]; then\n    # Stay in RAM")
	require.True(t, found)
	barrier, _, found = strings.Cut(barrier, "elif [ -n \"$SERVE_URL\" ]; then")
	require.True(t, found)
	// Execute just the handoff block. Mock network and sleep, never disk writes
	// or reboot. Delayed acknowledgment must resume from the same barrier.
	dir := t.TempDir()
	script := "set -eu\ncd '" + dir + "'\nMEDIA_CALLBACK=http://192.0.2.2/bootmedia/test\n" + `
log() { :; }
sleep() { :; }
curl() {
    for arg in "$@"; do last="$arg"; done
    case "$last" in
        */complete) marker=complete ;;
        */ready) marker=ready ;;
        *) exit 9 ;;
    esac
    if [ ! -f "$marker" ]; then
        touch "$marker"
        printf 503
    else
        printf 200
    fi
}
` + "# Stay in RAM" + barrier + "\n[ -f complete ] && [ -f ready ]\n"
	path := filepath.Join(dir, "barrier.sh")
	require.NoError(t, os.WriteFile(path, []byte(script), 0o600))
	require.NoError(t, runMediaCommand(t.Context(), "sh", path))
	require.Contains(t, barrier, "--max-time 15")
	require.Contains(t, barrier, `[ "$status" = 200 ]`)
	require.NotContains(t, barrier, "fatal")
	require.NotContains(t, barrier, "write_image")
}

func TestMediaFrozenArtifactsAfterPrepare(t *testing.T) {
	p, m, op := mediaFixture(t, "amd64")

	var calls []string

	p.Run = mockMediaRunner(t, &calls)
	id := op.Status.Targets[0].BootMedia.ID
	_, err := p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)

	frozen := op.Status.Targets[0].BootMedia.Netboot
	p.Resolver.Cache.SetDigestForArchitecture(frozen.Image, "amd64", "sha256:"+strings.Repeat("b", 64))
	require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("b", 64), map[string][]byte{"disk.img.gz": []byte("frozen disk")}))

	m.Spec.Netboot().Image = "changed/image:latest"
	m.Spec.Netboot().NetbootImage = "changed/boot:latest"
	m.Spec.Netboot().DHCPLeases[0].IPv4 = "198.51.100.20"
	m.Spec.Netboot().TargetDisk = "/dev/changed"
	m.Spec.Netboot().BootProtocol = "PXE"
	m.Status.Redfish = &v1alpha3.RedfishStatus{CertFingerprint: "changed-pin"}
	require.NoError(t, p.Client.Update(t.Context(), m))
	_, err = p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)
	setMediaPhase(t, p, op, "PoweringOn")

	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "ip=192.0.2.20::192.0.2.1")
	require.NotContains(t, w.Body.String(), "/dev/changed")

	h := &HTTPServer{Media: p, FileResolver: *p.Resolver}
	r := httptest.NewRequest(http.MethodGet, "/disk.img.gz", nil)
	r.RemoteAddr = "192.0.2.20:4321"
	w = httptest.NewRecorder()
	h.handleFile(w, r)
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "frozen disk", w.Body.String())

	require.NoError(t, p.Client.Get(t.Context(), client.ObjectKeyFromObject(op), op))
	op.Status.Phase = v1alpha3.OperationPhaseFailed
	require.NoError(t, p.Client.Status().Update(t.Context(), op))

	w = httptest.NewRecorder()
	h.handleFile(w, r)
	require.Equal(t, http.StatusNotFound, w.Code)
}

func TestMediaKernelHTTPRange(t *testing.T) {
	p, _, _ := mediaFixture(t, "arm64")
	require.NoError(t, populateOCICacheForArchitecture(p.CacheDir, strings.Repeat("a", 64), "arm64", map[string][]byte{"vmlinuz": []byte("gzip kernel payload")}))
	h := &HTTPServer{Media: p, FileResolver: *p.Resolver}

	for _, alias := range requestPathAliases("/vmlinuz") {
		t.Run(alias, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, alias, nil)
			r.RemoteAddr = "192.0.2.20:4321"
			r.Header.Set("Range", "bytes=5-10")

			w := httptest.NewRecorder()
			normalizePath(http.HandlerFunc(h.handleFile)).ServeHTTP(w, r)
			require.Equal(t, http.StatusPartialContent, w.Code)
			require.Equal(t, "bytes 5-10/19", w.Header().Get("Content-Range"))
			require.Equal(t, "kernel", w.Body.String())
		})
	}
}

func TestMediaIdentityAndOldImage(t *testing.T) {
	t.Run("old ISO cache", func(t *testing.T) {
		p, m, op := mediaFixture(t, "amd64")
		b := op.Status.Targets[0].BootMedia
		identity, err := json.Marshal([]any{mediaFormat, p.TFTPServer, p.Resolver.ServeURL, b.MachineUID, b.Netboot, b.ID})
		require.NoError(t, err)

		hash := sha256.Sum256(identity)
		dir := filepath.Join(p.CacheDir, "bootmedia", b.ID)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "identity"), []byte(hex.EncodeToString(hash[:])), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bootstrap.iso"), []byte("old non-hybrid ISO"), 0o600))
		_, err = p.Prepare(t.Context(), m, op, b.ID)
		require.ErrorContains(t, err, "immutable media inputs changed")
	})
	t.Run("identity", func(t *testing.T) {
		p, m, op := mediaFixture(t, "amd64")

		var calls []string

		p.Run = mockMediaRunner(t, &calls)
		m.Spec.Netboot().TargetDisk = "/dev/sdb"
		require.NoError(t, p.Client.Update(t.Context(), m))
		_, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.NoError(t, err)
		require.NoError(t, p.Client.Delete(t.Context(), m))
		m.ResourceVersion = ""
		m.UID = "replacement-uid"
		require.NoError(t, p.Client.Create(t.Context(), m))
		_, err = p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "identity")
	})
	t.Run("old image", func(t *testing.T) {
		p, m, op := mediaFixture(t, "amd64")
		meta, err := p.Resolver.Cache.MetadataForRefArchitecture(m.Spec.Netboot().NetbootImage, "amd64")
		require.NoError(t, err)

		meta.MediaFormat = ""
		_, err = p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
		require.ErrorContains(t, err, "lacks compatible")
	})
}

func TestMediaNoInstallHalts(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	id := op.Status.Targets[0].BootMedia.ID
	w := mediaRequest(p, id, http.MethodGet, "grub.cfg", "192.0.2.20")
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "halt\n", w.Body.String())
}

func TestMediaRenderingRequestsInstall(t *testing.T) {
	p, _, op := mediaFixture(t, "amd64")
	require.NoError(t, populateOCICache(p.CacheDir, strings.Repeat("a", 64), map[string][]byte{
		"grub/grub.cfg.tmpl": []byte("{{ .VirtualMedia }} {{ .InstallRequested }}"),
	}))
	o, err := p.owner(t.Context(), op.Status.Targets[0].BootMedia.ID)
	require.NoError(t, err)
	data, err := p.installerConfig(o)
	require.NoError(t, err)
	require.Equal(t, "true true", string(data))
}

func TestMediaEFIArtifacts(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for _, state := range []string{"legacy metadata", "missing shim", "missing grub", "template shim", "template grub"} {
			t.Run(arch+"/"+state, func(t *testing.T) {
				p, m, op := mediaFixture(t, arch)
				dir := p.Resolver.Cache.DiskDirForArchitecture("sha256:"+strings.Repeat("a", 64), arch)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "metadata.yaml"), []byte("mediaFormat: "+mediaFormat+"\nmediaShim: ignored\nmediaGrub: ignored\n"), 0o600))

				var calls []string

				p.Run = mockMediaRunner(t, &calls)
				id := op.Status.Targets[0].BootMedia.ID
				_, err := p.Prepare(t.Context(), m, op, id)
				require.NoError(t, err, "legacy metadata fields must remain accepted")

				count := len(calls)

				if state == "legacy metadata" {
					return
				}

				suffix := "x64.efi"
				if arch == "arm64" {
					suffix = "aa64.efi"
				}

				_, payload, _ := strings.Cut(state, " ")
				path := filepath.Join(dir, payload+suffix)
				require.NoError(t, os.Remove(path))

				if strings.HasPrefix(state, "template") {
					require.NoError(t, os.WriteFile(path+".tmpl", []byte("not an EFI binary"), 0o600))
				}

				_, err = p.Prepare(t.Context(), m, op, id)
				require.Error(t, err, "validate EFI files even when the ISO is cached")
				require.Len(t, calls, count)
			})
		}
	}
}

func TestInstallerTargetDiskExists(t *testing.T) {
	content, err := os.ReadFile("../../../images/netboot/assets/init")
	require.NoError(t, err)

	_, function, found := strings.Cut(string(content), "target_disk_exists() {")
	require.True(t, found)
	function, _, found = strings.Cut(function, "\n}\n")
	require.True(t, found)
	dir := t.TempDir()
	function = strings.ReplaceAll(function, "/dev/disk/by-id/", dir+"/by-id/")
	require.NoError(t, os.Mkdir(filepath.Join(dir, "by-id"), 0o700))

	script := "set -eu\ntarget_disk_exists() {" + function + "\n}\n" + `
calls=0
link_disk_ids() {
    calls=$((calls + 1))
    [ "$create" = no ] || : > "$TARGET_DISK"
}
TARGET_DISK=/dev/null
target_disk_exists
[ "$calls" -eq 0 ]
TARGET_DISK="$1/missing"
if target_disk_exists; then exit 1; fi
[ "$calls" -eq 0 ]
TARGET_DISK="$1/by-id/disk"
create=no
if target_disk_exists; then exit 1; fi
[ "$calls" -eq 1 ]
create=yes
target_disk_exists
[ "$calls" -eq 2 ]
target_disk_exists
[ "$calls" -eq 2 ]
`
	require.NoError(t, runMediaCommand(t.Context(), "sh", "-c", script, "target-disk-test", dir))
}

func TestNetbootContainerfileShellSyntax(t *testing.T) {
	content, err := os.ReadFile("../../../images/netboot/Containerfile")
	require.NoError(t, err)

	for _, line := range strings.Split(strings.ReplaceAll(string(content), "\\\n", ""), "\n") {
		if script, ok := strings.CutPrefix(line, "RUN "); ok {
			require.NoError(t, runMediaCommand(t.Context(), "sh", "-n", "-c", script))
		}
	}
}

func TestMediaRealToolsOptional(t *testing.T) {
	for _, tool := range []string{"mkfs.vfat", "mmd", "mcopy", "xorriso"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip("ISO packaging tool not installed: " + tool)
		}
	}

	p, m, op := mediaFixture(t, "amd64")
	source, err := p.Prepare(t.Context(), m, op, op.Status.Targets[0].BootMedia.ID)
	require.NoError(t, err)
	f, err := p.OpenISO(t.Context(), source.Name)
	require.NoError(t, err)

	defer f.Close()

	stat, err := f.Stat()
	require.NoError(t, err)
	require.Greater(t, stat.Size(), int64(8<<20))

	first, err := io.ReadAll(f)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	id := op.Status.Targets[0].BootMedia.ID
	require.NoError(t, p.Release(t.Context(), id))
	_, err = p.Prepare(t.Context(), m, op, id)
	require.NoError(t, err)
	second, err := os.ReadFile(filepath.Join(p.CacheDir, "bootmedia", id, "bootstrap.iso"))
	require.NoError(t, err)
	require.Equal(t, sha256.Sum256(first), sha256.Sum256(second), "regeneration must preserve ISO bytes")

	fat := assertHybridEFI(t, second)
	fatPath := filepath.Join(t.TempDir(), "efi.img")
	require.NoError(t, os.WriteFile(fatPath, fat, 0o600))
	// Inspect the FAT payload, not only command arguments.
	for _, path := range []string{"/EFI/BOOT/BOOTX64.EFI", "/EFI/BOOT/grubx64.efi", "/EFI/BOOT/grub.cfg", "/EFI/ubuntu/grub.cfg", "/boot/grub/grub.cfg", "/grub/grub.cfg", "/grub.cfg"} {
		name := filepath.Base(path)
		output := filepath.Join(t.TempDir(), name)
		require.NoError(t, runMediaCommand(t.Context(), "mcopy", "-i", fatPath, "::"+path, output))
		content, err := os.ReadFile(output)
		require.NoError(t, err)

		if name == "BOOTX64.EFI" {
			require.Equal(t, "unchanged shim amd64", string(content))
		}

		if name == "grubx64.efi" {
			require.Equal(t, "unchanged grub amd64", string(content))
		}

		if name == "grub.cfg" {
			require.Contains(t, string(content), "/bootmedia/"+id+"/grub.cfg")
		}
	}
}

func assertHybridEFI(t *testing.T, iso []byte) []byte {
	t.Helper()

	// GPT uses 512-byte sectors and stores the EFI System GUID in mixed endian.
	header := iso[512:1024]
	require.Equal(t, "EFI PART", string(header[:8]))
	entries := binary.LittleEndian.Uint64(header[72:80]) * 512
	require.GreaterOrEqual(t, binary.LittleEndian.Uint32(header[80:84]), uint32(2))
	entrySize := uint64(binary.LittleEndian.Uint32(header[84:88]))
	require.GreaterOrEqual(t, entrySize, uint64(128))
	require.LessOrEqual(t, entries+2*entrySize, uint64(len(iso)))
	partition := iso[entries+entrySize : entries+2*entrySize]
	require.Equal(t, "28732ac11ff8d211ba4b00a0c93ec93b", hex.EncodeToString(partition[:16]))
	start := binary.LittleEndian.Uint64(partition[32:40]) * 512
	end := (binary.LittleEndian.Uint64(partition[40:48]) + 1) * 512
	require.Less(t, start, end)
	require.LessOrEqual(t, end, uint64(len(iso)))
	fat := iso[start:end]
	require.Len(t, fat, 8<<20, "GPT partition 2 must contain the 8 MiB FAT image")
	require.Equal(t, "FAT16   ", string(fat[54:62]))
	require.Equal(t, byte(1), fat[13], "FAT must use one sector per cluster")

	// El Torito uses 2048-byte sectors. Its EFI entry must point to that same ESP.
	bootRecord := iso[17*2048 : 18*2048]
	require.Equal(t, byte(0), bootRecord[0])
	require.Equal(t, "CD001", string(bootRecord[1:6]))
	require.Equal(t, "EL TORITO SPECIFICATION", strings.TrimRight(string(bootRecord[7:39]), "\x00"))
	catalogOffset := uint64(binary.LittleEndian.Uint32(bootRecord[71:75])) * 2048
	require.LessOrEqual(t, catalogOffset+64, uint64(len(iso)))
	catalog := iso[catalogOffset : catalogOffset+64]
	require.Equal(t, byte(0xef), catalog[1], "El Torito platform must be EFI")
	require.Equal(t, byte(0x88), catalog[32], "El Torito entry must be bootable")
	require.Equal(t, byte(0), catalog[33], "El Torito must use no emulation")
	require.Equal(t, start, uint64(binary.LittleEndian.Uint32(catalog[40:44]))*2048)

	return fat
}
