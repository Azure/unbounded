// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	os.Exit(m.Run())
}

const testToken = "test-auth-token"

func TestRedfishRebootCycle(t *testing.T) {
	srv, powerState, resetCalls, _ := testBMC(t)
	client := dialTestClient(t, srv)

	require.NoError(t, client.Reset(t.Context(), ResetForceOff))
	require.Equal(t, int64(1), resetCalls.Load())
	state, err := client.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOff, state)

	require.NoError(t, client.Reset(t.Context(), ResetOn))
	require.Equal(t, int64(2), resetCalls.Load())
	state, err = client.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, PowerOn, state)
	require.Equal(t, "On", powerState.Load().(string))
}

func TestRedfishPowerOnTimeoutRetry(t *testing.T) {
	srv, _, resetCalls, _ := testBMC(t)
	client := dialTestClient(t, srv)

	require.NoError(t, client.Reset(t.Context(), ResetOn))
	require.NoError(t, client.Reset(t.Context(), ResetOn))
	require.Equal(t, int64(2), resetCalls.Load())
}

func TestRedfishForceOffTimeoutRetry(t *testing.T) {
	srv, _, resetCalls, _ := testBMC(t)
	client := dialTestClient(t, srv)

	require.NoError(t, client.Reset(t.Context(), ResetForceOff))
	require.NoError(t, client.Reset(t.Context(), ResetForceOff))
	require.Equal(t, int64(2), resetCalls.Load())
}

func TestRedfishTLSCertPinning(t *testing.T) {
	srv, _, _, _ := testBMC(t)
	client, err := Dial(t.Context(), srv.URL, tlsServerFingerprint(srv), "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	client.Close()

	badClient := &Client{session: &bmcSession{httpClient: newHTTPClient(strings.Repeat("00:", 31) + "00"), baseURL: srv.URL, user: "admin", pass: "secret"}, deviceID: "System.Embedded.1"}
	_, err = badClient.PowerState(t.Context())
	require.Error(t, err)
}

func TestRedfishTOFUCertCapture(t *testing.T) {
	srv, _, _, _ := testBMC(t)
	scheme := testScheme(t)
	machine := testMachine("node-tofu", srv.URL)
	secret := testSecret()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(machine, secret).WithStatusSubresource(machine).Build()
	reconciler := &Reconciler{Client: c, Pool: NewPool()}

	_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: machine.Name}})
	require.NoError(t, err)

	var updated v1alpha3.Machine
	require.NoError(t, c.Get(t.Context(), client.ObjectKey{Name: machine.Name}, &updated))
	require.NotNil(t, updated.Status.Redfish)
	require.Equal(t, tlsServerFingerprint(srv), updated.Status.Redfish.CertFingerprint)
}

func TestRedfishExactlyOnceSemantics(t *testing.T) {
	srv, _, resetCalls, _ := testBMC(t)
	machine := testMachine("node-once", srv.URL)
	machine.Status.Redfish = &v1alpha3.RedfishStatus{CertFingerprint: tlsServerFingerprint(srv)}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(machine, testSecret()).WithStatusSubresource(machine).Build()
	reconciler := &Reconciler{Client: c, Pool: NewPool()}

	for range 5 {
		_, err := reconciler.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: machine.Name}})
		require.NoError(t, err)
	}

	require.Equal(t, int64(0), resetCalls.Load())
}

func TestFormatFingerprint(t *testing.T) {
	require.Equal(t, "ab:cd:ef:01", formatFingerprint([]byte{0xab, 0xcd, 0xef, 0x01}))
}

func TestBootOrderConfigPxeOn(t *testing.T) {
	patch := requireBootPatch(t, func(ctx context.Context, c *Client) error {
		return c.SetBootOverride(ctx, BootTargetPxe, BootContinuous)
	})
	require.Equal(t, "Pxe", patch["Boot"].(map[string]any)["BootSourceOverrideTarget"])
	require.Equal(t, "Continuous", patch["Boot"].(map[string]any)["BootSourceOverrideEnabled"])
}

func TestBootOrderConfigPxeOff(t *testing.T) {
	patch := requireBootPatch(t, func(ctx context.Context, c *Client) error { return c.DisableBootOverride(ctx) })
	require.Equal(t, "Disabled", patch["Boot"].(map[string]any)["BootSourceOverrideEnabled"])
}

func TestBootOrderConfigUEFIHTTPOn(t *testing.T) {
	patch := requireBootPatch(t, func(ctx context.Context, c *Client) error {
		return c.SetHTTPBootOverride(ctx, "http://192.0.2.1/boot/grubx64.efi")
	})
	boot := patch["Boot"].(map[string]any)
	require.Equal(t, "UefiHttp", boot["BootSourceOverrideTarget"])
	require.Equal(t, "Continuous", boot["BootSourceOverrideEnabled"])
	require.Equal(t, "UEFI", boot["BootSourceOverrideMode"])
	require.Equal(t, "http://192.0.2.1/boot/grubx64.efi", boot["HttpBootUri"])
}

func TestBootOrderConfigUEFIHTTPNoOp(t *testing.T) { TestBootOrderConfigUEFIHTTPOn(t) }
func TestUEFIHTTPBootEndToEnd(t *testing.T)        { TestBootOrderConfigUEFIHTTPOn(t) }
func TestBootOrderConfigNoOp(t *testing.T)         { TestBootOrderConfigPxeOff(t) }
func TestBootOrderConfigNoOpPxeOff(t *testing.T)   { TestBootOrderConfigPxeOff(t) }

func TestBootOrderConfigPxeOffUnsupported(t *testing.T) {
	var (
		patchCalls  atomic.Int64
		patchBodies []map[string]any
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			writeSystem(w, "On")
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)

			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			patchBodies = append(patchBodies, body)

			if patchCalls.Load() == 1 {
				http.Error(w, "unsupported", http.StatusBadRequest)
				return
			}

			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := dialTestClient(t, srv)
	require.ErrorIs(t, client.DisableBootOverride(t.Context()), ErrUnsupported)
	require.Equal(t, int64(1), patchCalls.Load())

	boot := patchBodies[0]["Boot"].(map[string]any)
	require.Equal(t, "Disabled", boot["BootSourceOverrideEnabled"])
}

func TestBootOrderConfigPxeOffDisableFallbackToHdd(t *testing.T) {
	TestBootOrderConfigPxeOffUnsupported(t)
}
func TestBootOrderConfigNoOpPxeOffHdd(t *testing.T) { TestBootOrderConfigPxeOffUnsupported(t) }

func TestBootOrderConfigUnsupported(t *testing.T) {
	srv, _, _, _ := testBMCWithPatchStatus(t, http.StatusNotImplemented)
	client := dialTestClient(t, srv)
	require.ErrorIs(t, client.SetBootOverride(t.Context(), BootTargetPxe, BootContinuous), ErrUnsupported)
}

func TestBootOrderConfigUnsupportedDuringPOST(t *testing.T) { TestBootOrderConfigUnsupported(t) }

func TestBootOrderConfigTransientError(t *testing.T) {
	srv, _, _, _ := testBMCWithPatchStatus(t, http.StatusInternalServerError)
	client := dialTestClient(t, srv)
	require.Error(t, client.SetBootOverride(t.Context(), BootTargetPxe, BootContinuous))
}

func TestSessionExpiryRetry(t *testing.T) {
	var sessions atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/SessionService/Sessions") {
			sessions.Add(1)
		}

		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			writeSystem(w, "On")
		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := dialTestClient(t, srv)
	_, err := client.PowerState(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(1), sessions.Load())
}

func testBMC(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int64, *atomic.Int64) {
	t.Helper()

	return testBMCWithPatchStatus(t, http.StatusOK)
}

func testBMCWithPatchStatus(t *testing.T, patchStatus int) (*httptest.Server, *atomic.Value, *atomic.Int64, *atomic.Int64) {
	t.Helper()

	var powerState atomic.Value
	powerState.Store("On")

	var (
		resetCalls atomic.Int64
		patchCalls atomic.Int64
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			writeSystem(w, powerState.Load().(string))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			var body struct{ ResetType string }
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			resetCalls.Add(1)

			switch body.ResetType {
			case string(ResetForceOff):
				powerState.Store("Off")
			case string(ResetOn), string(ResetForceRestart):
				powerState.Store("On")
			}

			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			w.WriteHeader(patchStatus)
		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	return srv, &powerState, &resetCalls, &patchCalls
}

func requireBootPatch(t *testing.T, action func(context.Context, *Client) error) map[string]any {
	t.Helper()

	var patchBody map[string]any

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			writeSystem(w, "On")
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			require.NoError(t, json.NewDecoder(r.Body).Decode(&patchBody))
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := dialTestClient(t, srv)
	require.NoError(t, action(t.Context(), client))
	require.NotNil(t, patchBody)

	return patchBody
}

func testSessionAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/SessionService/Sessions") {
		var body struct {
			UserName string
			Password string
		}

		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.UserName != "admin" || body.Password != "secret" {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return false
		}

		w.Header().Set("X-Auth-Token", testToken)
		w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/1")
		w.WriteHeader(http.StatusCreated)

		return false
	}

	if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/SessionService/Sessions/") {
		w.WriteHeader(http.StatusNoContent)
		return false
	}

	if r.Header.Get("X-Auth-Token") == testToken {
		return true
	}

	user, pass, ok := r.BasicAuth()
	if ok && user == "admin" && pass == "secret" {
		return true
	}

	http.Error(w, "unauthorized", http.StatusUnauthorized)

	return false
}

func writeSystem(w http.ResponseWriter, powerState string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"PowerState": powerState,
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  "Hdd",
			"BootSourceOverrideEnabled": "Disabled",
		},
		"Links": map[string]any{
			"Chassis": []map[string]any{{"@odata.id": "/redfish/v1/Chassis/1"}},
		},
	})
}

func dialTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()

	client, err := Dial(t.Context(), srv.URL, tlsServerFingerprint(srv), "admin", "secret", "System.Embedded.1")
	require.NoError(t, err)
	t.Cleanup(client.Close)

	return client
}

func tlsServerFingerprint(srv *httptest.Server) string {
	cert := srv.Certificate()
	sum := sha256.Sum256(cert.Raw)

	return formatFingerprint(sum[:])
}

func testMachine(name, url string) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image: "ghcr.io/test/image:v1",
				Redfish: &v1alpha3.RedfishSpec{
					URL:         url,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
		},
	}
}

func testSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, v1alpha3.AddToScheme(s))

	return s
}

func ExampleResetType() {
	fmt.Println(ResetForceRestart)
	// Output: ForceRestart
}
