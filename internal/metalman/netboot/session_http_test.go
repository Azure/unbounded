// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package netboot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

func TestSessionHTTPServesImmutableArtifactWithCapabilityAndRange(t *testing.T) {
	t.Parallel()

	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	cache := setupOCICache(t, "unused.example/image:latest", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", map[string][]byte{
		"disk.img.gz": []byte("0123456789"),
	})
	require.NoError(t, populateOCICache(cache.CacheDir, "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", map[string][]byte{
		"disk.img.gz": []byte("mutable-tag-content"),
	}))
	cache.SetDigest("unused.example/image:latest", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")

	session := testNetbootSession("session-a", digest)
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	handler := (&SessionHTTPServer{Client: client, Cache: cache, Capabilities: signer}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-a/"+capability+"/artifacts/disk.img.gz", nil)
	request.Header.Set("Range", "bytes=2-5")

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusPartialContent, response.Code)
	require.Equal(t, "bytes 2-5/10", response.Header().Get("Content-Range"))
	require.Equal(t, "2345", response.Body.String())
}

func TestSessionHTTPRejectsInvalidExpiredAndUnlistedCapabilities(t *testing.T) {
	t.Parallel()

	const digest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	cache := setupOCICache(t, "unused.example/image:latest", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", map[string][]byte{
		"disk.img.gz": []byte("disk"),
		"secret":      []byte("secret"),
	})
	session := testNetbootSession("session-b", digest)
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	now := time.Unix(1_700_000_000, 0)
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time { return now })
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	handler := (&SessionHTTPServer{Client: client, Cache: cache, Capabilities: signer}).Handler()

	for name, test := range map[string]struct {
		path       string
		wantStatus int
	}{
		"invalid capability":  {path: "/v1/netboot/sessions/session-b/not-a-capability/artifacts/disk.img.gz", wantStatus: http.StatusUnauthorized},
		"tampered capability": {path: "/v1/netboot/sessions/session-b/" + capability + "x/artifacts/disk.img.gz", wantStatus: http.StatusUnauthorized},
		"unlisted artifact":   {path: "/v1/netboot/sessions/session-b/" + capability + "/artifacts/secret", wantStatus: http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, test.path, nil))
			require.Equal(t, test.wantStatus, response.Code)
		})
	}

	now = session.Spec.ExpiresAt.Add(time.Second)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-b/"+capability+"/artifacts/disk.img.gz", nil))
	require.Equal(t, http.StatusUnauthorized, response.Code)
	body, err := io.ReadAll(response.Result().Body)
	require.NoError(t, err)
	require.NotContains(t, string(body), capability)
}

func TestSessionHTTPRecordsAuthenticatedSessionCallback(t *testing.T) {
	t.Parallel()

	session := testNetbootSession("session-c", "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	recorder := &recordingSessionConditionRecorder{}
	handler := (&SessionHTTPServer{Client: client, Cache: NewOCICache(t.TempDir()), Capabilities: signer, StatusRecorder: recorder}).Handler()

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/netboot/sessions/session-c/"+capability+"/callbacks/boot-image-written", nil))

	require.Equal(t, http.StatusNoContent, response.Code)
	require.Equal(t, session.Name, recorder.sessionName)
	require.Equal(t, session.UID, recorder.sessionUID)
	require.Equal(t, v1alpha3.NetbootSessionConditionBootImageWritten, recorder.condition.Type)
	require.Equal(t, metav1.ConditionTrue, recorder.condition.Status)
}

func TestSessionHTTPRecordsCloudInitCompletionOnlyForFinalSuccess(t *testing.T) {
	t.Parallel()

	session := testNetbootSession("session-cloud-init", "sha256:cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd")
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	recorder := &recordingSessionConditionsRecorder{}
	handler := (&SessionHTTPServer{Client: client, Cache: NewOCICache(t.TempDir()), Capabilities: signer, StatusRecorder: recorder}).Handler()
	path := "/v1/netboot/sessions/session-cloud-init/" + capability + "/callbacks/cloud-init"

	for _, body := range []string{
		`{"event_type":"start","name":"modules-final","description":"running"}`,
		`{"event_type":"finish","name":"modules-config","description":"done","result":"SUCCESS"}`,
		`{"event_type":"finish","name":"modules-final","description":"done","result":"SUCCESS"}`,
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		require.Equal(t, http.StatusNoContent, response.Code)
	}

	require.Len(t, recorder.conditions, 3)
	require.Equal(t, metav1.ConditionFalse, recorder.conditions[0].Status)
	require.Equal(t, metav1.ConditionFalse, recorder.conditions[1].Status)
	require.Equal(t, metav1.ConditionTrue, recorder.conditions[2].Status)

	for _, condition := range recorder.conditions {
		require.Equal(t, v1alpha3.NetbootSessionConditionCloudInitDone, condition.Type)
	}
}

func TestSessionHTTPAttestsExactSessionMachineAndRecordsMilestone(t *testing.T) {
	t.Parallel()

	session := testNetbootSession("session-attest", "sha256:dededededededededededededededededededededededededededededededede")
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: session.Spec.Machine.Name, UID: session.Spec.Machine.UID}}
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session, machine).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	attester := &recordingSessionAttester{}
	recorder := &recordingSessionConditionRecorder{}
	handler := (&SessionHTTPServer{
		Client: client, Cache: NewOCICache(t.TempDir()), Capabilities: signer,
		StatusRecorder: recorder, Attestation: attester,
	}).Handler()

	request := httptest.NewRequest(http.MethodPost, "/v1/netboot/sessions/session-attest/"+capability+"/attest", strings.NewReader(`{"ekPub":"a2V5","srkPub":"a2V5"}`))
	request.RemoteAddr = "198.51.100.25:12345"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, machine.Name, attester.machine.Name)
	require.Equal(t, machine.UID, attester.machine.UID)
	require.Equal(t, v1alpha3.NetbootSessionConditionAttested, recorder.condition.Type)
	require.Equal(t, session.UID, recorder.sessionUID)
}

func TestSessionHTTPRecordsFirmwareDownloadForExactSession(t *testing.T) {
	t.Parallel()

	const digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	cache := setupOCICache(t, "unused.example/netboot:latest", strings.TrimPrefix(digest, "sha256:"), map[string][]byte{
		"bootx64.efi": []byte("firmware"),
	})
	session := testNetbootSession("session-firmware", digest)
	session.Spec.Boot.FirmwareArtifact = "bootx64.efi"
	session.Spec.Artifacts.Files = append(session.Spec.Artifacts.Files, v1alpha3.NetbootSessionArtifact{
		Name: "bootx64.efi", Source: "NetbootImage", Path: "/disk/bootx64.efi",
	})
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	recorder := &recordingSessionConditionRecorder{}

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-firmware/"+capability+"/artifacts/bootx64.efi", nil)
	(&SessionHTTPServer{Client: client, Cache: cache, Capabilities: signer, StatusRecorder: recorder}).Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, session.Name, recorder.sessionName)
	require.Equal(t, session.UID, recorder.sessionUID)
	require.Equal(t, v1alpha3.NetbootSessionConditionBootLoaderDownloaded, recorder.condition.Type)
}

func TestSessionArtifactURLUsesEndpointAndCapability(t *testing.T) {
	t.Parallel()

	session := testNetbootSession("session-url", "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	session.Spec.Endpoint.ExternalURL = "https://boot.example.com/base/"
	session.Spec.Boot.FirmwareArtifact = "http/bootx64.efi"
	session.Spec.Artifacts.Files = append(session.Spec.Artifacts.Files, v1alpha3.NetbootSessionArtifact{
		Name: "http/bootx64.efi", Source: "NetbootImage", Path: "/disk/http/bootx64.efi",
	})
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	bootURL, err := SessionArtifactURL(signer, session, session.Spec.Boot.FirmwareArtifact)
	require.NoError(t, err)
	require.Equal(t, "https://boot.example.com/base/v1/netboot/sessions/session-url/"+capability+"/artifacts/http/bootx64.efi", bootURL)
}

func TestSessionHTTPRendersBootArtifactsFromImmutableSnapshot(t *testing.T) {
	t.Parallel()

	const digest = "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"

	cache := NewOCICache(t.TempDir())
	diskDir := cache.DiskDirForArchitecture(digest, v1alpha3.PXEArchitectureAMD64)
	require.NoError(t, os.MkdirAll(filepath.Join(diskDir, "grub"), 0o755))
	templateContent, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", "grub.cfg.tmpl"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(diskDir, "grub", "grub.cfg.tmpl"), templateContent, 0o600))

	session := testNetbootSession("session-render", digest)
	session.Spec.Boot = v1alpha3.NetbootSessionBoot{
		Architecture: v1alpha3.PXEArchitectureAMD64,
		TargetDisk:   "/dev/sda",
		DHCPLeases: []v1alpha3.DHCPLease{{
			MAC: "aa:bb:cc:dd:ee:ff", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1",
		}},
	}
	session.Spec.Provisioning = v1alpha3.NetbootSessionProvisioning{
		Cluster: v1alpha3.NetbootSessionCluster{APIServerURL: "https://api.snapshot.example:6443"},
	}
	session.Spec.Artifacts.Files = append(session.Spec.Artifacts.Files,
		v1alpha3.NetbootSessionArtifact{Name: "grub/grub.cfg", Source: "NetbootImage", Path: "/disk/grub/grub.cfg"},
		v1alpha3.NetbootSessionArtifact{Name: "vmlinuz", Source: "NetbootImage", Path: "/disk/vmlinuz"},
		v1alpha3.NetbootSessionArtifact{Name: "initrd", Source: "NetbootImage", Path: "/disk/initrd"},
		v1alpha3.NetbootSessionArtifact{Name: "init.cpio", Source: "NetbootImage", Path: "/disk/init.cpio"},
	)
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-render/"+capability+"/artifacts/grub/grub.cfg", nil)
	(&SessionHTTPServer{Client: client, Cache: cache, Capabilities: signer}).Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	body := response.Body.String()
	capabilityBase := "https://boot.example.com/v1/netboot/sessions/session-render/" + capability
	require.Contains(t, body, "linux "+capabilityBase+"/artifacts/vmlinuz")
	require.Contains(t, body, "initrd "+capabilityBase+"/artifacts/initrd "+capabilityBase+"/artifacts/init.cpio")
	require.Contains(t, body, "unbounded.image_url="+capabilityBase+"/artifacts/disk.img.gz")
	require.Contains(t, body, "unbounded.serve_url="+capabilityBase)
	require.Contains(t, body, "unbounded.ds_url="+capabilityBase+"/artifacts/cloud-init/")
	require.Contains(t, body, "unbounded.apiserver_url=https://api.snapshot.example:6443")
	require.Contains(t, body, "unbounded.disk=/dev/sda")
}

func TestSessionHTTPRendersCapabilityScopedCallbacks(t *testing.T) {
	t.Parallel()

	const digest = "sha256:abababababababababababababababababababababababababababababababab"

	cache := NewOCICache(t.TempDir())
	diskDir := cache.DiskDirForArchitecture(digest, v1alpha3.PXEArchitectureAMD64)
	require.NoError(t, os.MkdirAll(filepath.Join(diskDir, "cloud-init"), 0o755))

	for _, artifact := range []string{"grub.cfg", "vendor-data"} {
		source, err := os.ReadFile(filepath.Join("..", "..", "..", "images", "netboot", "assets", artifact+".tmpl"))
		require.NoError(t, err)

		destination := filepath.Join(diskDir, artifact+".tmpl")
		if artifact == "vendor-data" {
			destination = filepath.Join(diskDir, "cloud-init", artifact+".tmpl")
		}

		require.NoError(t, os.WriteFile(destination, source, 0o600))
	}

	session := testNetbootSession("session-callbacks", digest)
	session.Spec.Boot.Architecture = v1alpha3.PXEArchitectureAMD64
	session.Spec.Artifacts.Files = append(session.Spec.Artifacts.Files,
		v1alpha3.NetbootSessionArtifact{Name: "grub.cfg", Source: "NetbootImage", Path: "/disk/grub.cfg"},
		v1alpha3.NetbootSessionArtifact{Name: "cloud-init/vendor-data", Source: "NetbootImage", Path: "/disk/cloud-init/vendor-data"},
	)
	client := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(session).Build()
	signer, err := NewCapabilitySigner([]byte("01234567890123456789012345678901"), "test-key", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	require.NoError(t, err)
	capability, err := signer.Sign(session)
	require.NoError(t, err)

	handler := (&SessionHTTPServer{Client: client, Cache: cache, Capabilities: signer}).Handler()
	capabilityBase := "https://boot.example.com/v1/netboot/sessions/session-callbacks/" + capability

	grubResponse := httptest.NewRecorder()
	handler.ServeHTTP(grubResponse, httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-callbacks/"+capability+"/artifacts/grub.cfg", nil))
	require.Equal(t, http.StatusOK, grubResponse.Code)
	require.Contains(t, grubResponse.Body.String(), "unbounded.boot_image_written_url="+capabilityBase+"/callbacks/boot-image-written")
	require.NotContains(t, grubResponse.Body.String(), "/pxe/disable")

	vendorResponse := httptest.NewRecorder()
	handler.ServeHTTP(vendorResponse, httptest.NewRequest(http.MethodGet, "/v1/netboot/sessions/session-callbacks/"+capability+"/artifacts/cloud-init/vendor-data", nil))
	require.Equal(t, http.StatusOK, vendorResponse.Code)
	require.Contains(t, vendorResponse.Body.String(), "endpoint: "+capabilityBase+"/callbacks/cloud-init")
	require.Contains(t, vendorResponse.Body.String(), `"URL": "`+capabilityBase+`"`)
	require.Contains(t, vendorResponse.Body.String(), `"`+capabilityBase+`/logs/agent-install"`)
}

type recordingSessionConditionRecorder struct {
	sessionName string
	sessionUID  types.UID
	condition   metav1.Condition
}

type recordingSessionConditionsRecorder struct {
	conditions []metav1.Condition
}

type recordingSessionAttester struct {
	machine *v1alpha3.Machine
}

func (a *recordingSessionAttester) AttestMachine(w http.ResponseWriter, _ *http.Request, machine *v1alpha3.Machine) {
	a.machine = machine.DeepCopy()

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"credentialBlob":"YQ=="}`))
}

func (r *recordingSessionConditionsRecorder) RecordCondition(_ context.Context, _ string, _ types.UID, condition metav1.Condition) error {
	r.conditions = append(r.conditions, condition)

	return nil
}

func (r *recordingSessionConditionRecorder) RecordCondition(_ context.Context, sessionName string, sessionUID types.UID, condition metav1.Condition) error {
	r.sessionName = sessionName
	r.sessionUID = sessionUID
	r.condition = condition

	return nil
}

func testNetbootSession(name, digest string) *v1alpha3.NetbootSession {
	return &v1alpha3.NetbootSession{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(name + "-uid")},
		Spec: v1alpha3.NetbootSessionSpec{
			Machine:   v1alpha3.NetbootSessionObjectSnapshot{Name: "machine-a", UID: "machine-uid", Generation: 1},
			Operation: v1alpha3.NetbootSessionObjectSnapshot{Name: "operation-a", UID: "operation-uid", Generation: 1},
			Endpoint:  v1alpha3.NetbootSessionEndpointSnapshot{Name: "endpoint-a", UID: "endpoint-uid", ExternalURL: "https://boot.example.com"},
			Boot:      v1alpha3.NetbootSessionBoot{Architecture: v1alpha3.PXEArchitectureAMD64},
			Artifacts: v1alpha3.NetbootSessionArtifacts{
				MachineImage: v1alpha3.NetbootSessionImage{Reference: "unused.example/image:latest", Digest: digest},
				NetbootImage: v1alpha3.NetbootSessionImage{Reference: "unused.example/netboot:latest", Digest: digest},
				Files:        []v1alpha3.NetbootSessionArtifact{{Name: "disk.img.gz", Source: "MachineImage", Path: "/disk/disk.img.gz"}},
			},
			ExpiresAt: metav1.NewTime(time.Unix(1_700_003_600, 0)),
		},
		Status: v1alpha3.NetbootSessionStatus{Phase: v1alpha3.NetbootSessionPhaseReady},
	}
}
