// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package attestation

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/go-tpm/tpm2"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakecr "sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/metalman/netboot"
	"github.com/Azure/unbounded/internal/unbounded"
)

// rsaKeyToTPM2BPublicEK creates a TPM2B_PUBLIC (EK-style) from an RSA public key.
// EKs have restricted+decrypt attributes and use OAEP with AES-128-CFB symmetric.
func rsaKeyToTPM2BPublicEK(t *testing.T, pub *rsa.PublicKey) []byte {
	t.Helper()

	tpmPub := tpm2.TPMTPublic{
		Type:    tpm2.TPMAlgRSA,
		NameAlg: tpm2.TPMAlgSHA256,
		ObjectAttributes: tpm2.TPMAObject{
			FixedTPM:            true,
			FixedParent:         true,
			SensitiveDataOrigin: true,
			UserWithAuth:        true,
			Restricted:          true,
			Decrypt:             true,
		},
		Parameters: tpm2.NewTPMUPublicParms(
			tpm2.TPMAlgRSA,
			&tpm2.TPMSRSAParms{
				Symmetric: tpm2.TPMTSymDefObject{
					Algorithm: tpm2.TPMAlgAES,
					KeyBits:   tpm2.NewTPMUSymKeyBits(tpm2.TPMAlgAES, tpm2.TPMKeyBits(128)),
					Mode:      tpm2.NewTPMUSymMode(tpm2.TPMAlgAES, tpm2.TPMAlgCFB),
				},
				KeyBits: tpm2.TPMKeyBits(pub.N.BitLen()),
			},
		),
		Unique: tpm2.NewTPMUPublicID(
			tpm2.TPMAlgRSA,
			&tpm2.TPM2BPublicKeyRSA{
				Buffer: pub.N.Bytes(),
			},
		),
	}
	encoded := tpm2.Marshal(tpmPub)
	buf := make([]byte, 2+len(encoded))
	binary.BigEndian.PutUint16(buf[:2], uint16(len(encoded)))
	copy(buf[2:], encoded)

	return buf
}

func testEKKeyPair(t *testing.T) (*rsa.PrivateKey, []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	return priv, rsaKeyToTPM2BPublicEK(t, &priv.PublicKey)
}

// testSRKPub generates a random SRK-like TPM2B_PUBLIC and its TPM Name.
func testSRKPub(t *testing.T) (srkPub, srkName []byte) {
	t.Helper()

	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	srkPub = rsaKeyToTPM2BPublicEK(t, &priv.PublicKey)

	tpm2BPub, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](srkPub)
	if err != nil {
		t.Fatal(err)
	}

	tpmPub, err := tpm2BPub.Contents()
	if err != nil {
		t.Fatal(err)
	}

	name, err := tpm2.ObjectName(tpmPub)
	if err != nil {
		t.Fatal(err)
	}

	return srkPub, name.Buffer
}

// testStatusUpdater implements StatusUpdater using a controller-runtime client.
type testStatusUpdater struct {
	client client.Client
}

func (u *testStatusUpdater) Update(ctx context.Context, node *v1alpha3.Machine) error {
	return u.client.Status().Update(ctx, node)
}

// testLookupByIP returns a LookupNodeByIP function that iterates Machines
// (the fake client doesn't support field indexes).
func testLookupByIP(c client.Client) func(ctx context.Context, ip string) (*v1alpha3.Machine, error) {
	return func(ctx context.Context, ip string) (*v1alpha3.Machine, error) {
		var list v1alpha3.MachineList
		if err := c.List(ctx, &list); err != nil {
			return nil, err
		}

		for i := range list.Items {
			if list.Items[i].Spec.PXE == nil {
				continue
			}

			for _, lease := range list.Items[i].Spec.PXE.DHCPLeases {
				if lease.IPv4 == ip {
					return &list.Items[i], nil
				}
			}
		}

		return nil, fmt.Errorf("no node found for IP %s", ip)
	}
}

func testHandler(t *testing.T, objects ...client.Object) *Handler {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := v1alpha3.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	crClient := fakecr.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(&v1alpha3.Machine{}).
		Build()

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: unbounded.SystemNamespace()},
	}
	clientset := fake.NewClientset(ns)

	return &Handler{
		Clientset:      clientset,
		LookupNodeByIP: testLookupByIP(crClient),
		ResolveBootstrapToken: func(context.Context, *v1alpha3.Machine) (string, error) {
			return "fake-bootstrap-token-for-testing", nil
		},
		StatusUpdater: &testStatusUpdater{client: crClient},
	}
}

func TestBootstrapTokenUsesMachineSecret(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: metav1.NamespaceSystem},
		Type:       corev1.SecretTypeBootstrapToken,
		Data: map[string][]byte{
			"token-id":     []byte("abc123"),
			"token-secret": []byte("0123456789abcdef"),
		},
	}
	handler := &Handler{Clientset: fake.NewClientset(secret)}
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
		Spec: v1alpha3.MachineSpec{Kubernetes: &v1alpha3.KubernetesSpec{
			BootstrapTokenRef: &v1alpha3.LocalObjectReference{Name: secret.Name},
		}},
	}

	token, err := handler.bootstrapToken(t.Context(), machine)
	require.NoError(t, err)
	require.Equal(t, "abc123.0123456789abcdef", token)
}

func TestBootstrapTokenRequiresMachineReference(t *testing.T) {
	handler := &Handler{Clientset: fake.NewClientset()}
	machine := &v1alpha3.Machine{ObjectMeta: metav1.ObjectMeta{Name: "machine-1"}}

	_, err := handler.bootstrapToken(t.Context(), machine)
	require.ErrorContains(t, err, "spec.kubernetes.bootstrapTokenRef.name is required")
}

func TestBootstrapTokenRejectsWrongSecretType(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: metav1.NamespaceSystem},
		Type:       corev1.SecretTypeOpaque,
	}
	handler := &Handler{Clientset: fake.NewClientset(secret)}
	machine := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
		Spec: v1alpha3.MachineSpec{Kubernetes: &v1alpha3.KubernetesSpec{
			BootstrapTokenRef: &v1alpha3.LocalObjectReference{Name: secret.Name},
		}},
	}

	_, err := handler.bootstrapToken(t.Context(), machine)
	require.ErrorContains(t, err, "expected \"bootstrap.kubernetes.io/token\"")
}

func TestBootstrapTokenRequiresTokenFields(t *testing.T) {
	tests := []struct {
		name      string
		data      map[string][]byte
		wantError string
	}{
		{
			name: "missing token ID",
			data: map[string][]byte{
				"token-secret": []byte("0123456789abcdef"),
			},
			wantError: "has no token-id",
		},
		{
			name: "missing token secret",
			data: map[string][]byte{
				"token-id": []byte("abc123"),
			},
			wantError: "has no token-secret",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-token-test", Namespace: metav1.NamespaceSystem},
				Type:       corev1.SecretTypeBootstrapToken,
				Data:       tt.data,
			}
			handler := &Handler{Clientset: fake.NewClientset(secret)}
			machine := &v1alpha3.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "machine-1"},
				Spec: v1alpha3.MachineSpec{Kubernetes: &v1alpha3.KubernetesSpec{
					BootstrapTokenRef: &v1alpha3.LocalObjectReference{Name: secret.Name},
				}},
			}

			_, err := handler.bootstrapToken(t.Context(), machine)
			require.ErrorContains(t, err, tt.wantError)
		})
	}
}

// doAttest performs an attest request from the given IP and returns the parsed response.
func doAttest(t *testing.T, handler *Handler, ip string, ekPub, srkPub []byte) AttestResponse {
	t.Helper()

	body, _ := json.Marshal(AttestRequest{
		EKPub:  ekPub,
		SRKPub: srkPub,
	})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("attest: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp AttestResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}

	return resp
}

// activateCredential implements TPM2_ActivateCredential in software,
// recovering the credential encrypted by CreateCredential.
// credentialBlob and encryptedSecret are expected in TPM2B format (2-byte size prefix).
func activateCredential(t *testing.T, ekPriv *rsa.PrivateKey, credentialBlob, encryptedSecret, srkName []byte) []byte {
	t.Helper()

	// Parse encryptedSecret: TPM2B(RSA-OAEP(seed))
	if len(encryptedSecret) < 2 {
		t.Fatal("encryptedSecret too short")
	}

	secretSize := binary.BigEndian.Uint16(encryptedSecret[:2])
	encSeed := encryptedSecret[2 : 2+secretSize]

	// Decrypt seed with RSA-OAEP.
	label := []byte("IDENTITY\x00")

	seed, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, ekPriv, encSeed, label)
	if err != nil {
		t.Fatalf("RSA-OAEP decrypt seed: %v", err)
	}

	// Parse credentialBlob: TPM2B(integrityDigest || encryptedCredential)
	if len(credentialBlob) < 2 {
		t.Fatal("credentialBlob too short")
	}

	blobSize := binary.BigEndian.Uint16(credentialBlob[:2])
	blobInner := credentialBlob[2 : 2+blobSize]

	// integrityDigest is TPM2B_DIGEST (2-byte size + HMAC)
	if len(blobInner) < 2 {
		t.Fatal("credentialBlob inner too short")
	}

	integritySize := binary.BigEndian.Uint16(blobInner[:2])
	integrity := blobInner[2 : 2+integritySize]
	encCredential := blobInner[2+integritySize:]

	// Derive HMAC key and verify integrity.
	hmacKey := tpm2.KDFa(crypto.SHA256, seed, "INTEGRITY", nil, nil, 256)
	mac := hmac.New(sha256.New, hmacKey)
	mac.Write(encCredential)
	mac.Write(srkName)

	expectedMAC := mac.Sum(nil)
	if !hmac.Equal(integrity, expectedMAC) {
		t.Fatal("credential integrity check failed")
	}

	// Derive AES key and decrypt credential.
	aesKey := tpm2.KDFa(crypto.SHA256, seed, "STORAGE", srkName, nil, 128)

	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("AES cipher: %v", err)
	}

	iv := make([]byte, aes.BlockSize)
	decrypted := make([]byte, len(encCredential))
	cipher.NewCFBDecrypter(block, iv).XORKeyStream(decrypted, encCredential) //nolint:staticcheck // CFB mode is mandated by the TPM 2.0 spec.

	// decrypted is TPM2B_DIGEST: 2-byte size + credential
	if len(decrypted) < 2 {
		t.Fatal("decrypted credential too short")
	}

	credSize := binary.BigEndian.Uint16(decrypted[:2])

	return decrypted[2 : 2+credSize]
}

func TestAttestTOFUStoresKey(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f0", IPv4: "10.0.1.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	handler := testHandler(t, node)

	// First attest should TOFU-store the EK public key.
	resp := doAttest(t, handler, "10.0.1.10", ekPub, srkPub)

	if len(resp.CredentialBlob) == 0 {
		t.Fatal("expected non-empty credential blob")
	}

	if len(resp.EncryptedSecret) == 0 {
		t.Fatal("expected non-empty encrypted secret")
	}

	if len(resp.EncryptedToken) == 0 {
		t.Fatal("expected non-empty encrypted token")
	}

	// Verify the EKPublicKey was stored on the Machine.
	updated, err := handler.LookupNodeByIP(t.Context(), "10.0.1.10")
	if err != nil {
		t.Fatal(err)
	}

	if updated.Status.TPM == nil || updated.Status.TPM.EKPublicKey == "" {
		t.Fatal("expected EKPublicKey to be stored after TOFU")
	}

	block, _ := pem.Decode([]byte(updated.Status.TPM.EKPublicKey))
	if block == nil || block.Type != "PUBLIC KEY" {
		t.Fatal("stored EKPub is not valid PEM PUBLIC KEY")
	}

	// Second attest with the same key should succeed.
	resp2 := doAttest(t, handler, "10.0.1.10", ekPub, srkPub)
	if len(resp2.CredentialBlob) == 0 {
		t.Fatal("expected second attest to succeed")
	}
}

func TestAttestTOFURejectsNewKey(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-tofu-reject"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f1", IPv4: "10.0.1.11", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	handler := testHandler(t, node)

	// First attest TOFU-stores the key.
	doAttest(t, handler, "10.0.1.11", ekPub, srkPub)

	// Generate a different EK key pair.
	_, differentEKPub := testEKKeyPair(t)

	// Attest with a different key should be rejected.
	body, _ := json.Marshal(AttestRequest{
		EKPub:  differentEKPub,
		SRKPub: srkPub,
	})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = "10.0.1.11:12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAttestRetainedMediaAfterLeaseEdit(t *testing.T) {
	ekPriv, ekPub := testEKKeyPair(t)
	srkPub, srkName := testSRKPub(t)
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "media-host", UID: "media-host-uid", Labels: map[string]string{"unbounded-cloud.io/site": "site-a"}},
		Spec: v1alpha3.MachineSpec{Host: &v1alpha3.HostSpec{Netboot: &v1alpha3.PXESpec{
			BootProtocol: v1alpha3.PXEBootProtocolVirtualMedia,
			DHCPLeases:   []v1alpha3.DHCPLease{{MAC: "02:11:22:33:44:55", IPv4: "192.0.2.20", SubnetMask: "255.255.255.0", Gateway: "192.0.2.1"}},
		}}},
	}
	now := metav1.Now()
	op := &v1alpha3.MachineOperation{
		ObjectMeta: metav1.ObjectMeta{Name: "replace", UID: "replace-uid"},
		Spec:       v1alpha3.MachineOperationSpec{OperationKind: v1alpha3.OperationHostReplace},
		Status: v1alpha3.MachineOperationStatus{Targets: []v1alpha3.MachineOperationTargetStatus{{
			MachineRef: node.Name,
			BootMedia: &v1alpha3.MachineOperationBootMediaStatus{
				MachineUID: node.UID, Site: "site-a", Netboot: node.Spec.Netboot().DeepCopy(),
				ImageWritten: true, RebootRequestedAt: &now, Released: true, Cleaned: true,
			},
		}}},
	}
	handler := testHandler(t, node, op)
	c := handler.StatusUpdater.(*testStatusUpdater).client
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(node), node))
	node.Spec.Netboot().DHCPLeases[0].IPv4 = "192.0.2.99"
	require.NoError(t, c.Update(t.Context(), node))
	provider := &netboot.MediaProvider{Client: c, Site: "site-a"}
	handler.LookupMachine = provider.LookupAttestationMachine
	handler.LookupNodeByIP = func(context.Context, string) (*v1alpha3.Machine, error) {
		t.Fatal("request lookup must take precedence over legacy IP lookup")
		return nil, fmt.Errorf("unexpected legacy lookup")
	}

	resp := doAttest(t, handler, "192.0.2.20", ekPub, srkPub)
	aesKey := activateCredential(t, ekPriv, resp.CredentialBlob, resp.EncryptedSecret, srkName)
	block, err := aes.NewCipher(aesKey)
	require.NoError(t, err)
	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)
	token, err := gcm.Open(nil, resp.GCMNonce, resp.EncryptedToken, nil)
	require.NoError(t, err)
	require.Equal(t, "fake-bootstrap-token-for-testing", string(token))
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(node), node))
	require.NotEmpty(t, node.Status.TPM.EKPublicKey)
	require.Equal(t, "192.0.2.99", node.Spec.Netboot().DHCPLeases[0].IPv4)

	body, err := json.Marshal(AttestRequest{EKPub: ekPub, SRKPub: srkPub})
	require.NoError(t, err)
	// A changed live site must fail without falling back to the legacy callback.
	node.Labels["unbounded-cloud.io/site"] = "site-b"
	require.NoError(t, c.Update(t.Context(), node))

	r := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	r.RemoteAddr = "192.0.2.20:4321"
	w := httptest.NewRecorder()
	handler.Attest(w, r)
	require.Equal(t, http.StatusNotFound, w.Code, w.Body.String())
}

func TestAttestEKMismatch(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)

	// Generate a DIFFERENT key pair and store its public key as EKPub PEM.
	otherPriv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	der, err := x509.MarshalPKIXPublicKey(&otherPriv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	otherPubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-03"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f2", IPv4: "10.0.1.12", SubnetMask: "255.255.255.0"}},
			},
		},
		Status: v1alpha3.MachineStatus{
			TPM: &v1alpha3.TPMStatus{EKPublicKey: otherPubPEM},
		},
	}

	handler := testHandler(t, node)

	body, _ := json.Marshal(AttestRequest{
		EKPub:  ekPub,
		SRKPub: srkPub,
	})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = "10.0.1.12:12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

// TestAttestE2E tests the full attest + ActivateCredential + decrypt flow.
func TestAttestE2E(t *testing.T) {
	ekPriv, ekPub := testEKKeyPair(t)
	srkPub, srkName := testSRKPub(t)

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-e2e"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f3", IPv4: "10.0.1.13", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	handler := testHandler(t, node)
	resp := doAttest(t, handler, "10.0.1.13", ekPub, srkPub)

	// Recover the AES key via software ActivateCredential.
	aesKey := activateCredential(t, ekPriv, resp.CredentialBlob, resp.EncryptedSecret, srkName)
	if len(aesKey) != 32 {
		t.Fatalf("expected 32-byte AES key, got %d", len(aesKey))
	}

	// Decrypt the bootstrap token.
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		t.Fatalf("AES cipher: %v", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("GCM: %v", err)
	}

	token, err := gcm.Open(nil, resp.GCMNonce, resp.EncryptedToken, nil)
	if err != nil {
		t.Fatalf("GCM decrypt: %v", err)
	}

	if string(token) == "" {
		t.Fatal("expected non-empty bootstrap token")
	}
}

func TestAttestNodeNotFound(t *testing.T) {
	handler := testHandler(t)

	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)

	body, _ := json.Marshal(AttestRequest{
		EKPub:  ekPub,
		SRKPub: srkPub,
	})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = "10.99.99.99:12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestAttestMissingFields(t *testing.T) {
	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-badreq"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:d0", IPv4: "10.0.1.60", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	handler := testHandler(t, node)

	body, _ := json.Marshal(AttestRequest{
		EKPub:  nil,
		SRKPub: nil,
	})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = "10.0.1.60:12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

// TestCreateCredentialRoundTrip tests that CreateCredential output can be recovered
// by activateCredential (software TPM2_ActivateCredential).
func TestCreateCredentialRoundTrip(t *testing.T) {
	priv, ekPub := testEKKeyPair(t)
	_, srkName := testSRKPub(t)

	// Import EK as a LabeledEncapsulationKey.
	ekTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](ekPub)
	if err != nil {
		t.Fatal(err)
	}

	ekTPMPub, err := ekTPM2B.Contents()
	if err != nil {
		t.Fatal(err)
	}

	ekEncKey, err := tpm2.ImportEncapsulationKey(ekTPMPub)
	if err != nil {
		t.Fatal(err)
	}

	credential := make([]byte, 32)
	if _, err := rand.Read(credential); err != nil {
		t.Fatal(err)
	}

	idObject, encSecret, err := tpm2.CreateCredential(rand.Reader, ekEncKey, srkName, credential)
	if err != nil {
		t.Fatalf("CreateCredential: %v", err)
	}

	// Wrap in TPM2B for activateCredential (which expects the wire format).
	credBlob := make([]byte, 2+len(idObject))
	binary.BigEndian.PutUint16(credBlob[:2], uint16(len(idObject)))
	copy(credBlob[2:], idObject)

	encSecretWrapped := make([]byte, 2+len(encSecret))
	binary.BigEndian.PutUint16(encSecretWrapped[:2], uint16(len(encSecret)))
	copy(encSecretWrapped[2:], encSecret)

	recovered := activateCredential(t, priv, credBlob, encSecretWrapped, srkName)
	if !bytes.Equal(recovered, credential) {
		t.Fatalf("recovered credential does not match: got %x, want %x", recovered, credential)
	}
}

// TestCreateCredentialWrongKey verifies that activateCredential fails with a different EK.
func TestCreateCredentialWrongKey(t *testing.T) {
	_, ekPub1 := testEKKeyPair(t)
	priv2, _ := testEKKeyPair(t)
	_, srkName := testSRKPub(t)

	// Import EK1 as a LabeledEncapsulationKey.
	ekTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](ekPub1)
	if err != nil {
		t.Fatal(err)
	}

	ekTPMPub, err := ekTPM2B.Contents()
	if err != nil {
		t.Fatal(err)
	}

	ekEncKey, err := tpm2.ImportEncapsulationKey(ekTPMPub)
	if err != nil {
		t.Fatal(err)
	}

	credential := []byte("test-credential")

	_, encSecret, err := tpm2.CreateCredential(rand.Reader, ekEncKey, srkName, credential)
	if err != nil {
		t.Fatal(err)
	}

	// Try to decrypt the seed with the wrong key - RSA-OAEP decrypt should fail.
	label := []byte("IDENTITY\x00")

	_, decryptErr := rsa.DecryptOAEP(sha256.New(), rand.Reader, priv2, encSecret, label)
	if decryptErr == nil {
		t.Fatal("expected RSA-OAEP decrypt to fail with wrong key")
	}
}

func newPXEMachine(name, mac, ip string) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: mac, IPv4: ip, SubnetMask: "255.255.255.0"}},
			},
		},
	}
}

func rawAttest(handler *Handler, ip string, ekPub, srkPub []byte) *httptest.ResponseRecorder {
	body, _ := json.Marshal(AttestRequest{EKPub: ekPub, SRKPub: srkPub})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = ip + ":12345"
	w := httptest.NewRecorder()
	handler.Attest(w, req)

	return w
}

// countingStatusUpdater wraps a StatusUpdater, counting calls and optionally failing.
type countingStatusUpdater struct {
	inner StatusUpdater
	calls int
	err   error
	seen  []*v1alpha3.Machine
}

func (u *countingStatusUpdater) Update(ctx context.Context, node *v1alpha3.Machine) error {
	u.calls++
	u.seen = append(u.seen, node.DeepCopy())

	if u.err != nil {
		return u.err
	}

	return u.inner.Update(ctx, node)
}

func TestAttestStoresEKInOneUpdate(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)
	handler := testHandler(t, newPXEMachine("bind-01", "aa:bb:cc:dd:ee:10", "10.0.2.10"))
	inner := handler.StatusUpdater.(*testStatusUpdater)
	counter := &countingStatusUpdater{inner: inner}
	handler.StatusUpdater = counter

	doAttest(t, handler, "10.0.2.10", ekPub, srkPub)

	require.Equal(t, 1, counter.calls)
	require.NotNil(t, counter.seen[0].Status.TPM)
	require.NotEmpty(t, counter.seen[0].Status.TPM.EKPublicKey)

	var m v1alpha3.Machine
	require.NoError(t, inner.client.Get(t.Context(), client.ObjectKey{Name: "bind-01"}, &m))
	require.NotEmpty(t, m.Status.TPM.EKPublicKey)
}

func TestAttestStatusUpdateFailureWithholdsToken(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)
	handler := testHandler(t, newPXEMachine("bind-04", "aa:bb:cc:dd:ee:13", "10.0.2.13"))
	handler.StatusUpdater = &countingStatusUpdater{
		inner: handler.StatusUpdater,
		err:   fmt.Errorf("conflict"),
	}

	w := rawAttest(handler, "10.0.2.13", ekPub, srkPub)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "encryptedToken")
}

func TestAttestStatusConflictWithholdsToken(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)
	handler := testHandler(t, newPXEMachine("bind-05", "aa:bb:cc:dd:ee:14", "10.0.2.14"))
	c := handler.StatusUpdater.(*testStatusUpdater).client
	lookup := handler.LookupNodeByIP
	// Return a stale copy, then bump the stored object so the update conflicts.
	handler.LookupNodeByIP = func(ctx context.Context, ip string) (*v1alpha3.Machine, error) {
		m, err := lookup(ctx, ip)
		if err != nil {
			return nil, err
		}

		live := m.DeepCopy()
		live.Labels = map[string]string{"bump": "1"}
		require.NoError(t, c.Update(ctx, live))

		return m, nil
	}

	w := rawAttest(handler, "10.0.2.14", ekPub, srkPub)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.NotContains(t, w.Body.String(), "encryptedToken")
}

func TestAttestEKMismatchMintsNoToken(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	_, otherEKPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)
	handler := testHandler(t, newPXEMachine("bind-06", "aa:bb:cc:dd:ee:15", "10.0.2.15"))
	doAttest(t, handler, "10.0.2.15", ekPub, srkPub)

	var calls int

	handler.ResolveBootstrapToken = func(context.Context, *v1alpha3.Machine) (string, error) {
		calls++

		return "should-not-resolve", nil
	}

	w := rawAttest(handler, "10.0.2.15", otherEKPub, srkPub)
	require.Equal(t, http.StatusForbidden, w.Code)
	require.Equal(t, 0, calls)
}

func TestAttestIgnoresForwardedHeaders(t *testing.T) {
	_, ekPub := testEKKeyPair(t)
	srkPub, _ := testSRKPub(t)

	node := &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "node-01"},
		Spec: v1alpha3.MachineSpec{
			PXE: &v1alpha3.PXESpec{
				DHCPLeases: []v1alpha3.DHCPLease{{MAC: "aa:bb:cc:dd:ee:f0", IPv4: "10.0.1.10", SubnetMask: "255.255.255.0"}},
			},
		},
	}

	handler := testHandler(t, node)

	body, _ := json.Marshal(AttestRequest{EKPub: ekPub, SRKPub: srkPub})
	req := httptest.NewRequest(http.MethodPost, "/attest", bytes.NewReader(body))
	req.RemoteAddr = "10.99.99.99:12345"
	req.Header.Set("X-Forwarded-For", "10.0.1.10")
	req.Header.Set("X-Real-IP", "10.0.1.10")
	req.Header.Set("Forwarded", "for=10.0.1.10")

	w := httptest.NewRecorder()
	handler.Attest(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("spoofed forwarded headers: expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
