// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package attestation

import (
	"bytes"
	"context"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-tpm/tpm2"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/machinestatus"
)

const (
	BootstrapSAName      = "metalman-bootstrap"
	BootstrapSANamespace = "unbounded-kube"

	// maxRequestBodySize limits attestation request bodies to 1 MiB.
	maxRequestBodySize = 1 << 20
)

type Handler struct {
	Clientset      kubernetes.Interface
	ClusterCA      []byte // PEM-encoded cluster CA certificate
	LookupNodeByIP func(ctx context.Context, ip string) (*v1alpha3.Machine, error)
	StatusUpdater  StatusUpdater
	Recorder       events.EventRecorder
}

// StatusUpdater updates the status subresource of a Machine.
type StatusUpdater interface {
	Update(ctx context.Context, node *v1alpha3.Machine) error
	UpdateStatus(ctx context.Context, key client.ObjectKey, mutate func(*v1alpha3.Machine) bool) error
}

type AttestRequest struct {
	EKPub  []byte `json:"ekPub"`  // TPM2B_PUBLIC: EK public key
	SRKPub []byte `json:"srkPub"` // TPM2B_PUBLIC: SRK public key (from tpm2_readpublic)
}

type AttestResponse struct {
	CredentialBlob  []byte `json:"credentialBlob"`      // MakeCredential output: HMAC + encrypted credential
	EncryptedSecret []byte `json:"encryptedSecret"`     // RSA-OAEP encrypted seed
	EncryptedToken  []byte `json:"encryptedToken"`      // AES-256-GCM encrypted bootstrap token
	GCMNonce        []byte `json:"gcmNonce"`            // AES-GCM nonce
	ClusterCA       string `json:"clusterCA,omitempty"` // PEM-encoded CA certificate
}

// clientIP extracts the client IP from an HTTP request, checking
// X-Forwarded-For first, then falling back to RemoteAddr.
func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.SplitN(fwd, ",", 2)
		return strings.TrimSpace(parts[0])
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// Attest handles the single-round-trip TPM attestation flow.
// The node sends its EK and SRK public keys. The server wraps a random
// AES-256 key via CreateCredential (bound to the EK and SRK), encrypts a
// bootstrap token with that key, and returns both. Only the node with
// the matching TPM can recover the AES key and decrypt the token.
func (h *Handler) Attest(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)

	var req AttestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		slog.Warn("attest: invalid request body", "remote", r.RemoteAddr, "err", err)
		http.Error(w, "invalid request body", http.StatusBadRequest)

		return
	}

	ip := clientIP(r)

	log := slog.With("remote", r.RemoteAddr, "ip", ip)
	if len(req.EKPub) == 0 || len(req.SRKPub) == 0 {
		log.Warn("attest: missing fields",
			"hasEKPub", len(req.EKPub) > 0,
			"hasSRKPub", len(req.SRKPub) > 0,
		)
		http.Error(w, "missing or invalid fields", http.StatusBadRequest)

		return
	}

	ctx := r.Context()

	node, err := h.LookupNodeByIP(ctx, ip)
	if err != nil {
		log.Warn("attest: no Machine for client IP", "err", err)
		http.Error(w, "node not found", http.StatusNotFound)

		return
	}

	log = log.With("node", node.Name)
	log.Info("attest: processing request")

	// Decode EK and SRK TPM2B_PUBLIC structures.
	ekTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](req.EKPub)
	if err != nil {
		log.Error("attest: decoding EK TPM2B_PUBLIC", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKDecodeFailed", fmt.Sprintf("failed to decode EK public key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	ekTPMPub, err := ekTPM2B.Contents()
	if err != nil {
		log.Error("attest: extracting EK TPMT_PUBLIC", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKDecodeFailed", fmt.Sprintf("failed to extract EK public key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	ekKey, err := tpm2.Pub(*ekTPMPub)
	if err != nil {
		log.Error("attest: extracting EK crypto key", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKDecodeFailed", fmt.Sprintf("failed to extract EK crypto key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	// TOFU: store or verify the EK public key.
	storedEKPublicKey := ""
	if node.Status.TPM != nil {
		storedEKPublicKey = node.Status.TPM.EKPublicKey
	}

	if storedEKPublicKey == "" {
		storedEKPublicKey, err = h.storeEKPublicKey(ctx, node, ekKey)
		if err != nil {
			log.Error("attest: storing EK public key", "err", err)
			h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "StatusUpdateFailed", fmt.Sprintf("failed to store EK public key: %v", err), corev1.EventTypeWarning)
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		log.Info("TOFU: stored EK public key")
	}

	if storedEKPublicKey != "" {
		block, _ := pem.Decode([]byte(storedEKPublicKey))
		if block == nil {
			log.Error("attest: invalid stored EK public key PEM")
			h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "StoredEKInvalid", "stored EK public key is not valid PEM", corev1.EventTypeWarning)
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		storedKey, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			log.Error("attest: parsing stored EK public key", "err", err)
			h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "StoredEKInvalid", fmt.Sprintf("stored EK public key could not be parsed: %v", err), corev1.EventTypeWarning)
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		if !publicKeysEqual(ekKey, storedKey) {
			log.Warn("attest: EK public key does not match stored key")
			h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKMismatch", "EK public key does not match the stored Machine TPM identity", corev1.EventTypeWarning)
			http.Error(w, "EK public key does not match stored key", http.StatusForbidden)

			return
		}
	}

	// Decode SRK TPM2B_PUBLIC and compute its Name for CreateCredential.
	srkTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](req.SRKPub)
	if err != nil {
		log.Error("attest: decoding SRK TPM2B_PUBLIC", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "SRKDecodeFailed", fmt.Sprintf("failed to decode SRK public key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	srkTPMPub, err := srkTPM2B.Contents()
	if err != nil {
		log.Error("attest: extracting SRK TPMT_PUBLIC", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "SRKDecodeFailed", fmt.Sprintf("failed to extract SRK public key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	srkName, err := tpm2.ObjectName(srkTPMPub)
	if err != nil {
		log.Error("attest: computing SRK Name", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "SRKDecodeFailed", fmt.Sprintf("failed to compute SRK name: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	// Convert the EK public key to a LabeledEncapsulationKey for CreateCredential.
	ekEncKey, err := tpm2.ImportEncapsulationKey(ekTPMPub)
	if err != nil {
		log.Error("attest: importing EK encapsulation key", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKDecodeFailed", fmt.Sprintf("failed to import EK encapsulation key: %v", err), corev1.EventTypeWarning)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	// Generate a random AES-256 key and wrap it via CreateCredential.
	// Only the node's TPM can recover this key.
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		log.Error("attest: generating AES key", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "CredentialFailed", fmt.Sprintf("failed to generate attestation secret: %v", err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	idObject, encSecret, err := tpm2.CreateCredential(rand.Reader, ekEncKey, srkName.Buffer, aesKey)
	if err != nil {
		log.Error("attest: CreateCredential", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "CredentialFailed", fmt.Sprintf("failed to create TPM credential: %v", err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Wrap idObject and encSecret in TPM2B (2-byte length prefix) for the
	// client, which passes them to tpm2_activatecredential in tpm2-tools
	// file format.
	credentialBlob := make([]byte, 2+len(idObject))
	binary.BigEndian.PutUint16(credentialBlob[:2], uint16(len(idObject)))
	copy(credentialBlob[2:], idObject)

	encryptedSecret := make([]byte, 2+len(encSecret))
	binary.BigEndian.PutUint16(encryptedSecret[:2], uint16(len(encSecret)))
	copy(encryptedSecret[2:], encSecret)

	// Create a short-lived bootstrap token.
	tokenRequest := &authenticationv1.TokenRequest{
		Spec: authenticationv1.TokenRequestSpec{
			ExpirationSeconds: ptr.To(int64(3600)),
		},
	}

	tokenResp, err := h.Clientset.CoreV1().ServiceAccounts(BootstrapSANamespace).CreateToken(
		ctx, BootstrapSAName, tokenRequest, metav1.CreateOptions{},
	)
	if err != nil {
		log.Error("attest: creating SA token", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "TokenRequestFailed", fmt.Sprintf("failed to request bootstrap token for %s/%s: %v", BootstrapSANamespace, BootstrapSAName, err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Encrypt the bootstrap token with the credential-wrapped AES key.
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		log.Error("attest: creating AES cipher", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "CredentialFailed", fmt.Sprintf("failed to create bootstrap token cipher: %v", err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Error("attest: creating GCM", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "CredentialFailed", fmt.Sprintf("failed to create bootstrap token encryption: %v", err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	gcmNonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, gcmNonce); err != nil {
		log.Error("attest: generating GCM nonce", "err", err)
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "CredentialFailed", fmt.Sprintf("failed to generate bootstrap token nonce: %v", err), corev1.EventTypeWarning)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	encryptedToken := gcm.Seal(nil, gcmNonce, []byte(tokenResp.Status.Token), nil)

	tokenHash := sha256.Sum256([]byte(tokenResp.Status.Token))
	log.Info("attest: issued encrypted bootstrap token", "tokenID", fmt.Sprintf("%x", tokenHash[:8]))
	h.setAttestationCondition(ctx, node, metav1.ConditionTrue, "Issued", fmt.Sprintf("issued encrypted bootstrap token for service account %s/%s", BootstrapSANamespace, BootstrapSAName), corev1.EventTypeNormal)

	resp := AttestResponse{
		CredentialBlob:  credentialBlob,
		EncryptedSecret: encryptedSecret,
		EncryptedToken:  encryptedToken,
		GCMNonce:        gcmNonce,
		ClusterCA:       string(h.ClusterCA),
	}

	w.Header().Set("Content-Type", "application/json")

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error("attest: writing response", "err", err)
	}
}

func (h *Handler) setAttestationCondition(ctx context.Context, node *v1alpha3.Machine, status metav1.ConditionStatus, reason, message, eventType string) {
	if node == nil || h.StatusUpdater == nil {
		return
	}

	changed := false

	if err := h.StatusUpdater.UpdateStatus(ctx, client.ObjectKeyFromObject(node), func(latest *v1alpha3.Machine) bool {
		changed = machinestatus.SetConditionIfChanged(latest, machinestatus.Condition(
			v1alpha3.MachineConditionAttestationReady,
			status,
			reason,
			message,
			latest.Generation,
		))

		return changed
	}); err != nil {
		slog.Error("attest: updating attestation condition", "node", node.Name, "err", err)
		return
	}

	if !changed {
		return
	}

	machinestatus.Event(h.Recorder, node, eventType, "Attestation"+reason, message)
}

func (h *Handler) storeEKPublicKey(ctx context.Context, node *v1alpha3.Machine, ekKey crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(ekKey)
	if err != nil {
		h.setAttestationCondition(ctx, node, metav1.ConditionFalse, "EKMarshalFailed", fmt.Sprintf("failed to store EK public key: %v", err), corev1.EventTypeWarning)

		return "", err
	}

	ekPubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

	if err := h.StatusUpdater.UpdateStatus(ctx, client.ObjectKeyFromObject(node), func(latest *v1alpha3.Machine) bool {
		if latest.Status.TPM != nil && latest.Status.TPM.EKPublicKey != "" {
			ekPubPEM = latest.Status.TPM.EKPublicKey

			return false
		}

		if latest.Status.TPM == nil {
			latest.Status.TPM = &v1alpha3.TPMStatus{}
		}

		latest.Status.TPM.EKPublicKey = ekPubPEM

		return true
	}); err != nil {
		return "", err
	}

	return ekPubPEM, nil
}

// publicKeysEqual compares two crypto.PublicKey values structurally.
func publicKeysEqual(a, b crypto.PublicKey) bool {
	type equaler interface {
		Equal(crypto.PublicKey) bool
	}
	if ae, ok := a.(equaler); ok {
		return ae.Equal(b)
	}

	aDER, err := x509.MarshalPKIXPublicKey(a)
	if err != nil {
		return false
	}

	bDER, err := x509.MarshalPKIXPublicKey(b)
	if err != nil {
		return false
	}

	return bytes.Equal(aDER, bDER)
}

// ClusterCAFromConfig extracts the cluster CA certificate from a rest.Config.
func ClusterCAFromConfig(cfg *rest.Config) []byte {
	if len(cfg.CAData) > 0 {
		return cfg.CAData
	}

	if cfg.CAFile != "" {
		data, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			slog.Error("reading cluster CA file", "path", cfg.CAFile, "err", err)
			return nil
		}

		return data
	}

	return nil
}
