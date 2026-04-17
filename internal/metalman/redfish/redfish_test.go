// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
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
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded-kube/api/machina/v1alpha3"
)

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.FromSlogHandler(slog.Default().Handler()))
	os.Exit(m.Run())
}

const testToken = "test-auth-token"

// testSessionAuth handles Redfish session creation, deletion, and
// authentication for test BMC servers. Returns true if the request is
// authenticated and should be processed further, or false if the response
// has already been written (session lifecycle or auth failure).
func testSessionAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/SessionService/Sessions") {
		var body struct {
			UserName string `json:"UserName"`
			Password string `json:"Password"`
		}
		json.NewDecoder(r.Body).Decode(&body)

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

func TestRedfishRebootCycle(t *testing.T) {
	var powerState atomic.Value
	powerState.Store("On")

	var forceOffCalls, onCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": powerState.Load().(string),
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/Chassis.Embedded.1"},
					},
				},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			var body struct {
				ResetType string `json:"ResetType"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			switch body.ResetType {
			case "ForceOff":
				forceOffCalls.Add(1)
				powerState.Store("Off")
			case "On":
				onCalls.Add(1)
				powerState.Store("On")
			}

			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f0", IPv4: "10.0.0.1", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 1,
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-01", Namespace: "default"}}

	// First reconcile: machine is On, should send ForceOff and set
	// PoweredOff=False with Reason=PoweringOff.
	var updated v1alpha3.Machine

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	if forceOffCalls.Load() != 1 {
		t.Fatalf("expected 1 ForceOff call, got %d", forceOffCalls.Load())
	}

	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	poweredOffCond := meta.FindStatusCondition(updated.Status.Conditions, condPoweredOff)
	if poweredOffCond == nil || poweredOffCond.Status != metav1.ConditionFalse || poweredOffCond.Reason != "PoweringOff" {
		t.Fatalf("expected PoweredOff=False/PoweringOff, got %+v", poweredOffCond)
	}

	// Second reconcile: machine still not off - should NOT send ForceOff again,
	// just requeue.
	powerState.Store("On")

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if forceOffCalls.Load() != 1 {
		t.Fatalf("expected no additional ForceOff call, got %d", forceOffCalls.Load())
	}

	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting for power off")
	}

	// Third reconcile: machine is now Off, should set PoweredOff=True.
	powerState.Store("Off")

	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 3: %v", err)
	}

	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	if !meta.IsStatusConditionTrue(updated.Status.Conditions, condPoweredOff) {
		t.Fatal("expected PoweredOff=True after ForceOff completed")
	}

	// Fourth reconcile: PoweredOff is True, machine still Off, should send On
	// and update condition reason to PoweringOn.
	powerState.Store("Off")

	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 4: %v", err)
	}

	if onCalls.Load() != 1 {
		t.Fatalf("expected 1 On call, got %d", onCalls.Load())
	}

	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	if !meta.IsStatusConditionTrue(updated.Status.Conditions, condPoweredOff) {
		t.Fatal("expected PoweredOff=True to persist after sending On")
	}

	poweredOffCond = meta.FindStatusCondition(updated.Status.Conditions, condPoweredOff)
	if poweredOffCond == nil || poweredOffCond.Reason != "PoweringOn" {
		t.Fatalf("expected PoweredOff reason=PoweringOn, got %+v", poweredOffCond)
	}

	// Fifth reconcile: machine still Off but On was already sent - should not
	// send On again, just requeue.
	powerState.Store("Off")

	result, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 5: %v", err)
	}

	if onCalls.Load() != 1 {
		t.Fatalf("expected no additional On call, got %d", onCalls.Load())
	}

	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while waiting for power on")
	}

	// Simulate machine completing power-on.
	powerState.Store("On")

	// Sixth reconcile: machine is On + PoweredOff=True, clear condition and increment reboots.
	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 6: %v", err)
	}

	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	if meta.IsStatusConditionTrue(updated.Status.Conditions, condPoweredOff) {
		t.Fatal("expected PoweredOff to be cleared after verifying power On")
	}

	if updated.Status.Operations == nil || updated.Status.Operations.RebootCounter != 1 {
		var got int64
		if updated.Status.Operations != nil {
			got = updated.Status.Operations.RebootCounter
		}

		t.Fatalf("expected operations.rebootCounter=1, got %d", got)
	}

	repavedCond := meta.FindStatusCondition(updated.Status.Conditions, condRepaved)
	if repavedCond == nil || repavedCond.Status != metav1.ConditionFalse || repavedCond.Reason != "Pending" {
		t.Fatalf("expected Repaved=False/Pending, got %+v", repavedCond)
	}

	if repavedCond.Message != "image=ghcr.io/test/test-image:v1" {
		t.Fatalf("expected Repaved message 'image=ghcr.io/test/test-image:v1', got %q", repavedCond.Message)
	}

	// Seventh reconcile: reboots match - no-op (timeout is handled by the
	// lifecycle controller, not the redfish reconciler).
	prevForceOff := forceOffCalls.Load()
	prevOn := onCalls.Load()

	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 7: %v", err)
	}

	if forceOffCalls.Load() != prevForceOff || onCalls.Load() != prevOn {
		t.Fatal("expected no additional Redfish calls after reboot completed")
	}
}

func TestRedfishPowerOnTimeoutRetry(t *testing.T) {
	var powerState atomic.Value
	powerState.Store("Off")

	var onCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": powerState.Load().(string),
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/Chassis.Embedded.1"},
					},
				},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			var body struct {
				ResetType string `json:"ResetType"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			if body.ResetType == "On" {
				onCalls.Add(1)
			}

			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	// Machine is stuck in PoweringOn state with a stale LastTransitionTime
	// that exceeds powerActionTimeout - this reproduces the deadlock from
	// bug.yaml where the On command was lost and never retried.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-stuck", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f5", IPv4: "10.0.0.6", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 22,
				RepaveCounter: 22,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish:    &v1alpha3.RedfishStatus{CertFingerprint: fp},
			Operations: &v1alpha3.OperationsStatus{RebootCounter: 21, RepaveCounter: 20},
			Conditions: []metav1.Condition{
				{
					Type:               condPoweredOff,
					Status:             metav1.ConditionTrue,
					Reason:             "PoweringOn",
					Message:            "target reboots: 22",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-stuck", Namespace: "default"}}

	// Reconcile should detect the timeout and retry the On command instead
	// of silently requeueing forever.
	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if onCalls.Load() != 1 {
		t.Fatalf("expected On command to be retried after timeout, got %d calls", onCalls.Load())
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, condPoweredOff)
	if cond == nil {
		t.Fatal("expected PoweredOff condition to exist after retry")
	}

	if cond.Reason != "PoweringOn" {
		t.Fatalf("expected reason PoweringOn, got %s", cond.Reason)
	}

	// LastTransitionTime should have been reset (much more recent than the
	// stale 48-hour-old timestamp).
	if time.Since(cond.LastTransitionTime.Time) > time.Minute {
		t.Fatalf("expected LastTransitionTime to be reset, but it is %v old", time.Since(cond.LastTransitionTime.Time))
	}
}

func TestRedfishForceOffTimeoutRetry(t *testing.T) {
	var powerState atomic.Value
	powerState.Store("On")

	var forceOffCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": powerState.Load().(string),
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/Chassis.Embedded.1"},
					},
				},
			})

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			var body struct {
				ResetType string `json:"ResetType"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			if body.ResetType == "ForceOff" {
				forceOffCalls.Add(1)
			}

			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	// Machine is stuck in PoweringOff state with a stale LastTransitionTime.
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-stuck-off", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f6", IPv4: "10.0.0.7", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
			Conditions: []metav1.Condition{
				{
					Type:               condPoweredOff,
					Status:             metav1.ConditionFalse,
					Reason:             "PoweringOff",
					Message:            "target reboots: 1",
					LastTransitionTime: metav1.NewTime(time.Now().Add(-10 * time.Minute)),
				},
				{
					Type:   condBootSupported,
					Status: metav1.ConditionFalse,
					Reason: "NotSupported",
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-stuck-off", Namespace: "default"}}

	// Reconcile should detect the timeout and retry the ForceOff command.
	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if forceOffCalls.Load() != 1 {
		t.Fatalf("expected ForceOff to be retried after timeout, got %d calls", forceOffCalls.Load())
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	cond := meta.FindStatusCondition(updated.Status.Conditions, condPoweredOff)
	if cond == nil {
		t.Fatal("expected PoweredOff condition to exist after retry")
	}

	if cond.Reason != "PoweringOff" {
		t.Fatalf("expected reason PoweringOff, got %s", cond.Reason)
	}

	if time.Since(cond.LastTransitionTime.Time) > time.Minute {
		t.Fatalf("expected LastTransitionTime to be reset, but it is %v old", time.Since(cond.LastTransitionTime.Time))
	}
}

func TestRedfishTLSCertPinning(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"PowerState": "On"})
	}))
	defer srv.Close()

	wrongFP := formatFingerprint(make([]byte, 32))
	s := &bmcSession{
		httpClient: newHTTPClient(wrongFP),
		baseURL:    srv.URL,
	}

	_, _, err := s.do(t.Context(), http.MethodGet, "/redfish/v1/Systems/1", nil)
	if err == nil {
		t.Fatal("expected TLS cert pinning error, got nil")
	}

	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected mismatch error, got: %v", err)
	}
}

func TestRedfishTOFUCertCapture(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	expectedFP := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-tofu", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f1", IPv4: "10.0.0.2", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-tofu", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	if updated.Status.Redfish == nil || updated.Status.Redfish.CertFingerprint == "" {
		t.Fatal("expected TLS cert fingerprint to be populated after TOFU")
	}

	if updated.Status.Redfish.CertFingerprint != expectedFP {
		t.Fatalf("fingerprint mismatch: got %s, want %s", updated.Status.Redfish.CertFingerprint, expectedFP)
	}
}

func TestRedfishExactlyOnceSemantics(t *testing.T) {
	var resetCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]string{"PowerState": "On"})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/Actions/ComputerSystem.Reset"):
			resetCalls.Add(1)
			w.WriteHeader(http.StatusOK)
		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-once", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f4", IPv4: "10.0.0.5", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RebootCounter: 3,
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish:    &v1alpha3.RedfishStatus{CertFingerprint: fp},
			Operations: &v1alpha3.OperationsStatus{RebootCounter: 3},
			Conditions: []metav1.Condition{
				{
					Type:   condBootSupported,
					Status: metav1.ConditionFalse,
					Reason: "NotSupported",
				},
			},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-once", Namespace: "default"}}

	for i := range 5 {
		_, err := reconciler.Reconcile(ctx, req)
		if err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}

	if resetCalls.Load() != 0 {
		t.Fatalf("expected 0 reset calls when reboots == observedReboots, got %d", resetCalls.Load())
	}
}

func TestFormatFingerprint(t *testing.T) {
	input := []byte{0xab, 0xcd, 0xef, 0x01}
	got := formatFingerprint(input)

	want := "ab:cd:ef:01"
	if got != want {
		t.Fatalf("FormatFingerprint = %q, want %q", got, want)
	}
}

func TestBootOrderConfigPxeOn(t *testing.T) {
	var (
		patchCalls atomic.Int64
		patchBody  map[string]any
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Hdd",
					"BootSourceOverrideEnabled": "Once",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			json.NewDecoder(r.Body).Decode(&patchBody)
			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-pxe", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b0", IPv4: "10.0.0.30", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-pxe", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", patchCalls.Load())
	}

	boot, ok := patchBody["Boot"].(map[string]any)
	if !ok {
		t.Fatal("expected Boot in PATCH body")
	}

	if boot["BootSourceOverrideTarget"] != "Pxe" {
		t.Fatalf("expected BootSourceOverrideTarget=Pxe, got %v", boot["BootSourceOverrideTarget"])
	}

	if boot["BootSourceOverrideEnabled"] != "Continuous" {
		t.Fatalf("expected BootSourceOverrideEnabled=Continuous, got %v", boot["BootSourceOverrideEnabled"])
	}
}

func TestBootOrderConfigPxeOff(t *testing.T) {
	var (
		patchCalls atomic.Int64
		patchBody  map[string]any
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			json.NewDecoder(r.Body).Decode(&patchBody)
			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-hdd", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b1", IPv4: "10.0.0.31", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-hdd", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 1 {
		t.Fatalf("expected 1 PATCH call, got %d", patchCalls.Load())
	}

	boot, ok := patchBody["Boot"].(map[string]any)
	if !ok {
		t.Fatal("expected Boot in PATCH body")
	}

	if _, hasTarget := boot["BootSourceOverrideTarget"]; hasTarget {
		t.Fatalf("expected no BootSourceOverrideTarget, got %v", boot["BootSourceOverrideTarget"])
	}

	if boot["BootSourceOverrideEnabled"] != "Disabled" {
		t.Fatalf("expected BootSourceOverrideEnabled=Disabled, got %v", boot["BootSourceOverrideEnabled"])
	}
}

func TestBootOrderConfigNoOp(t *testing.T) {
	var patchCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-noop", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b2", IPv4: "10.0.0.32", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-noop", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 0 {
		t.Fatalf("expected 0 PATCH calls for no-op, got %d", patchCalls.Load())
	}
}

func TestBootOrderConfigNoOpPxeOff(t *testing.T) {
	var patchCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "None",
					"BootSourceOverrideEnabled": "Disabled",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-noop-off", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b5", IPv4: "10.0.0.35", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-noop-off", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 0 {
		t.Fatalf("expected 0 PATCH calls for no-op, got %d", patchCalls.Load())
	}
}

func TestBootOrderConfigPxeOffDisableFallbackToHdd(t *testing.T) {
	var (
		patchCalls    atomic.Int64
		lastPatchBody map[string]any
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Pxe",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)

			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			lastPatchBody = body

			boot, _ := body["Boot"].(map[string]any)
			if boot["BootSourceOverrideEnabled"] == "Disabled" {
				w.WriteHeader(http.StatusBadRequest)
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

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-fallback", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b6", IPv4: "10.0.0.36", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-fallback", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 2 {
		t.Fatalf("expected 2 PATCH calls (Disabled then Hdd fallback), got %d", patchCalls.Load())
	}

	boot, ok := lastPatchBody["Boot"].(map[string]any)
	if !ok {
		t.Fatal("expected Boot in last PATCH body")
	}

	if boot["BootSourceOverrideTarget"] != "Hdd" {
		t.Fatalf("expected fallback BootSourceOverrideTarget=Hdd, got %v", boot["BootSourceOverrideTarget"])
	}

	if boot["BootSourceOverrideEnabled"] != "Continuous" {
		t.Fatalf("expected fallback BootSourceOverrideEnabled=Continuous, got %v", boot["BootSourceOverrideEnabled"])
	}
}

func TestBootOrderConfigNoOpPxeOffHdd(t *testing.T) {
	var patchCalls atomic.Int64

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Hdd",
					"BootSourceOverrideEnabled": "Continuous",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			patchCalls.Add(1)
			w.WriteHeader(http.StatusOK)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-noop-hdd", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b7", IPv4: "10.0.0.37", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-noop-hdd", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if patchCalls.Load() != 0 {
		t.Fatalf("expected 0 PATCH calls for Hdd+Continuous no-op, got %d", patchCalls.Load())
	}
}

func TestBootOrderConfigUnsupported(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "None",
					"BootSourceOverrideEnabled": "Disabled",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			http.NotFound(w, r)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-unsup", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b3", IPv4: "10.0.0.33", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-unsup", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	bootCond := meta.FindStatusCondition(updated.Status.Conditions, condBootSupported)
	if bootCond == nil {
		t.Fatal("expected BootOrderConfigSupported condition to be set")
	}

	if bootCond.Status != metav1.ConditionFalse {
		t.Fatalf("expected BootOrderConfigSupported=False, got %s", bootCond.Status)
	}

	if bootCond.Reason != "NotSupported" {
		t.Fatalf("expected reason NotSupported, got %s", bootCond.Reason)
	}
}

func TestBootOrderConfigUnsupportedDuringPOST(t *testing.T) {
	var powerState atomic.Value
	powerState.Store("PoweringOn")

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": powerState.Load().(string),
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "None",
					"BootSourceOverrideEnabled": "Disabled",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			http.NotFound(w, r)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-post", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b8", IPv4: "10.0.0.38", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-post", Namespace: "default"}}

	// First reconcile: system is POSTing (PoweringOn), boot order PATCH
	// returns 404. Should NOT set the unsupported condition - just requeue.
	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 1: %v", err)
	}

	if result.RequeueAfter == 0 {
		t.Fatal("expected requeue while system is in transient power state")
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	bootCond := meta.FindStatusCondition(updated.Status.Conditions, condBootSupported)
	if bootCond != nil {
		t.Fatalf("expected BootOrderConfigSupported condition NOT to be set during POST, got %+v", bootCond)
	}

	// Simulate POST completing - system is now On.
	powerState.Store("On")

	// Second reconcile: system is On, boot order PATCH still returns 404.
	// Should now set the unsupported condition.
	_, err = reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("reconcile 2: %v", err)
	}

	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	bootCond = meta.FindStatusCondition(updated.Status.Conditions, condBootSupported)
	if bootCond == nil {
		t.Fatal("expected BootOrderConfigSupported condition to be set after system reached stable state")
	}

	if bootCond.Status != metav1.ConditionFalse {
		t.Fatalf("expected BootOrderConfigSupported=False, got %s", bootCond.Status)
	}

	if bootCond.Reason != "NotSupported" {
		t.Fatalf("expected reason NotSupported, got %s", bootCond.Reason)
	}
}

func TestBootOrderConfigTransientError(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !testSessionAuth(w, r) {
			return
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			json.NewEncoder(w).Encode(map[string]any{
				"PowerState": "On",
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "None",
					"BootSourceOverrideEnabled": "Disabled",
				},
				"Links": map[string]any{
					"Chassis": []map[string]any{
						{"@odata.id": "/redfish/v1/Chassis/1"},
					},
				},
			})

		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/Systems/System.Embedded.1"):
			w.WriteHeader(http.StatusServiceUnavailable)

		case strings.Contains(r.URL.Path, "/TrustedComponents"):
			http.NotFound(w, r)

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bmc-pass", Namespace: "default"},
		Data:       map[string][]byte{"password": []byte("secret")},
	}

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-boot-503", Namespace: "default"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				Image:      "ghcr.io/test/test-image:v1",
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:b4", IPv4: "10.0.0.34", SubnetMask: "255.255.255.0"}},
				Redfish: &v1alpha3.RedfishSpec{
					URL:         srv.URL,
					Username:    "admin",
					DeviceID:    "System.Embedded.1",
					PasswordRef: v1alpha3.SecretKeySelector{Name: "bmc-pass", Namespace: "default", Key: "password"},
				},
			},
			Operations: &v1alpha3.OperationsSpec{
				RepaveCounter: 1,
			},
		},
		Status: v1alpha3.MachineStatus{
			Redfish: &v1alpha3.RedfishStatus{CertFingerprint: fp},
		},
	}

	scheme := testScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(node, secret).
		WithStatusSubresource(node).
		Build()

	reconciler := &Reconciler{Client: fc, Pool: NewPool()}
	ctx := t.Context()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "node-boot-503", Namespace: "default"}}

	_, err := reconciler.Reconcile(ctx, req)
	if err == nil {
		t.Fatal("expected transient error to be returned, got nil")
	}

	var updated v1alpha3.Machine
	if err := fc.Get(ctx, req.NamespacedName, &updated); err != nil {
		t.Fatal(err)
	}

	bootCond := meta.FindStatusCondition(updated.Status.Conditions, condBootSupported)
	if bootCond != nil {
		t.Fatalf("expected BootOrderConfigSupported condition NOT to be set on transient error, got %+v", bootCond)
	}
}

func TestSessionExpiryRetry(t *testing.T) {
	var (
		sessionCount atomic.Int64
		currentToken atomic.Value
	)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Session creation - assigns a unique token per session.
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/SessionService/Sessions") {
			var body struct {
				UserName string `json:"UserName"`
				Password string `json:"Password"`
			}
			json.NewDecoder(r.Body).Decode(&body)

			if body.UserName != "admin" || body.Password != "secret" {
				http.Error(w, "bad credentials", http.StatusUnauthorized)
				return
			}

			n := sessionCount.Add(1)
			token := fmt.Sprintf("token-%d", n)
			currentToken.Store(token)

			w.Header().Set("X-Auth-Token", token)
			w.Header().Set("Location", "/redfish/v1/SessionService/Sessions/1")
			w.WriteHeader(http.StatusCreated)

			return
		}

		if r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/SessionService/Sessions/") {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Authenticate: token must match the current valid token.
		tok := r.Header.Get("X-Auth-Token")

		valid, _ := currentToken.Load().(string)
		if tok != valid {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "admin" || pass != "secret" {
				http.Error(w, "session expired", http.StatusUnauthorized)
				return
			}
		}

		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Systems/1"):
			json.NewEncoder(w).Encode(map[string]string{"PowerState": "On"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	fp := tlsServerFingerprint(srv)

	pool := NewPool()
	defer pool.Close()

	ctx := t.Context()

	c, err := pool.Get(ctx, srv.URL, fp, "admin", "secret", "1")
	if err != nil {
		t.Fatal(err)
	}

	// Request succeeds with the initial session token.
	state, err := c.PowerState(ctx)
	if err != nil {
		t.Fatalf("initial request failed: %v", err)
	}

	if state != "On" {
		t.Fatalf("expected power state On, got %s", state)
	}

	// Simulate BMC session timeout by invalidating the current token.
	currentToken.Store("expired-by-bmc")

	// The client's cached token is now stale. The next request should
	// get a 401, transparently re-authenticate, and retry successfully.
	state, err = c.PowerState(ctx)
	if err != nil {
		t.Fatalf("request after session expiry failed (expected transparent retry): %v", err)
	}

	if state != "On" {
		t.Fatalf("expected power state On after re-auth, got %s", state)
	}

	// Exactly two sessions should have been created: the initial Dial
	// and the automatic re-authentication after expiry.
	if got := sessionCount.Load(); got != 2 {
		t.Fatalf("expected 2 sessions (initial + reauth), got %d", got)
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	s := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}

	return s
}

func tlsServerFingerprint(srv *httptest.Server) string {
	raw := srv.TLS.Certificates[0].Certificate[0]
	h := sha256.Sum256(raw)

	return formatFingerprint(h[:])
}
