// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

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
	"encoding/base64"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/unbounded"
)

const (
	BootstrapSAName = "metalman-bootstrap"

	// maxRequestBodySize limits attestation request bodies to 1 MiB.
	maxRequestBodySize = 1 << 20
)

type Handler struct {
	Clientset      kubernetes.Interface
	ClusterCA      []byte // PEM-encoded cluster CA certificate
	LookupNodeByIP func(ctx context.Context, ip string) (*v1alpha3.Machine, error)
	// LookupMachine takes precedence over LookupNodeByIP when set.
	LookupMachine func(r *http.Request) (*v1alpha3.Machine, error)
	StatusUpdater StatusUpdater
}

// StatusUpdater updates the status subresource of a Machine.
type StatusUpdater interface {
	Update(ctx context.Context, node *v1alpha3.Machine) error
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

	var (
		node *v1alpha3.Machine
		err  error
	)

	if h.LookupMachine != nil {
		node, err = h.LookupMachine(r)
	} else {
		node, err = h.LookupNodeByIP(ctx, ip)
	}

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
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	ekTPMPub, err := ekTPM2B.Contents()
	if err != nil {
		log.Error("attest: extracting EK TPMT_PUBLIC", "err", err)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	ekKey, err := tpm2.Pub(*ekTPMPub)
	if err != nil {
		log.Error("attest: extracting EK crypto key", "err", err)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	// TOFU: record or verify the EK public key. On first use the pin is
	// only staged on the in-memory object here; it is persisted together
	// with the bootstrap credential ID in a single status update after the
	// token is minted (see below). A mismatched EK is rejected before any
	// token is minted.
	if node.Status.TPM == nil || node.Status.TPM.EKPublicKey == "" {
		der, err := x509.MarshalPKIXPublicKey(ekKey)
		if err != nil {
			log.Error("attest: marshaling EK public key", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		ekPubPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))

		if node.Status.TPM == nil {
			node.Status.TPM = &v1alpha3.TPMStatus{}
		}

		node.Status.TPM.EKPublicKey = ekPubPEM

		log.Info("TOFU: staging EK public key")
	} else {
		block, _ := pem.Decode([]byte(node.Status.TPM.EKPublicKey))
		if block == nil {
			log.Error("attest: invalid stored EK public key PEM")
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		storedKey, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			log.Error("attest: parsing stored EK public key", "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)

			return
		}

		if !publicKeysEqual(ekKey, storedKey) {
			log.Warn("attest: EK public key does not match stored key")
			http.Error(w, "EK public key does not match stored key", http.StatusForbidden)

			return
		}
	}

	// Decode SRK TPM2B_PUBLIC and compute its Name for CreateCredential.
	srkTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](req.SRKPub)
	if err != nil {
		log.Error("attest: decoding SRK TPM2B_PUBLIC", "err", err)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	srkTPMPub, err := srkTPM2B.Contents()
	if err != nil {
		log.Error("attest: extracting SRK TPMT_PUBLIC", "err", err)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	srkName, err := tpm2.ObjectName(srkTPMPub)
	if err != nil {
		log.Error("attest: computing SRK Name", "err", err)
		http.Error(w, "invalid SRK public key", http.StatusBadRequest)

		return
	}

	// Convert the EK public key to a LabeledEncapsulationKey for CreateCredential.
	ekEncKey, err := tpm2.ImportEncapsulationKey(ekTPMPub)
	if err != nil {
		log.Error("attest: importing EK encapsulation key", "err", err)
		http.Error(w, "invalid EK public key", http.StatusBadRequest)

		return
	}

	// Generate a random AES-256 key and wrap it via CreateCredential.
	// Only the node's TPM can recover this key.
	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		log.Error("attest: generating AES key", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	idObject, encSecret, err := tpm2.CreateCredential(rand.Reader, ekEncKey, srkName.Buffer, aesKey)
	if err != nil {
		log.Error("attest: CreateCredential", "err", err)
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

	tokenResp, err := h.Clientset.CoreV1().ServiceAccounts(unbounded.SystemNamespace()).CreateToken(
		ctx, BootstrapSAName, tokenRequest, metav1.CreateOptions{},
	)
	if err != nil {
		log.Error("attest: creating SA token", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Bind the token to this Machine. The machina CSR approver only approves
	// kubelet client CSRs from the bootstrap ServiceAccount whose
	// credential-id matches a Machine's status.bootstrap.credentialID, so the
	// credential ID must be persisted before the token is handed out. Fail
	// closed if it cannot be determined or stored.
	credentialID, err := CredentialIDFromToken(tokenResp.Status.Token)
	if err != nil {
		log.Error("attest: bootstrap token has no usable jti claim; the API server must issue "+
			"ServiceAccount tokens with a jti (Kubernetes 1.30+, ServiceAccountTokenJTI)", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Re-attestation overwrites any previous credential ID, so tokens issued
	// by earlier attestations can no longer get kubelet client certificates
	// approved for this Machine.
	issuedAt := metav1.Now()
	node.Status.Bootstrap = &v1alpha3.BootstrapStatus{
		CredentialID: credentialID,
		IssuedAt:     &issuedAt,
	}

	// Single status update: persists the TOFU EK pin (if staged above) and
	// the bootstrap credential ID together. A conflict or any other error
	// aborts without returning the token.
	if err := h.StatusUpdater.Update(ctx, node); err != nil {
		log.Error("attest: recording bootstrap credential on Machine status", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	// Encrypt the bootstrap token with the credential-wrapped AES key.
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		log.Error("attest: creating AES cipher", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		log.Error("attest: creating GCM", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	gcmNonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, gcmNonce); err != nil {
		log.Error("attest: generating GCM nonce", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)

		return
	}

	encryptedToken := gcm.Seal(nil, gcmNonce, []byte(tokenResp.Status.Token), nil)

	tokenHash := sha256.Sum256([]byte(tokenResp.Status.Token))
	log.Info("attest: issued encrypted bootstrap token", "tokenID", fmt.Sprintf("%x", tokenHash[:8]))

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

// CredentialIDFromToken extracts the jti claim from a ServiceAccount JWT and
// returns it in the form kube-apiserver reports in the
// "authentication.kubernetes.io/credential-id" user extra ("JTI=<jti>").
// The signature is not verified; the token was just minted by the API server.
func CredentialIDFromToken(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("token is not a JWT: expected 3 segments, got %d", len(parts))
	}

	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("decoding JWT payload: %w", err)
	}

	var claims struct {
		JTI any `json:"jti"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("parsing JWT claims: %w", err)
	}

	jti, ok := claims.JTI.(string)
	if !ok || jti == "" {
		return "", fmt.Errorf("JWT has no string jti claim")
	}

	return "JTI=" + jti, nil
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
