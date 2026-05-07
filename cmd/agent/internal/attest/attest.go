// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package attest

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"time"

	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/google/go-tpm/tpm2/transport/linuxtpm"

	"github.com/Azure/unbounded/internal/provision"
	"github.com/Azure/unbounded/pkg/agent/goalstates"
	"github.com/Azure/unbounded/pkg/agent/phases"
)

const (
	// tpmDevicePath is the Linux resource-manager device path.
	// The kernel resource manager (/dev/tpmrm0) multiplexes access and
	// automatically cleans up transient objects when the file descriptor
	// is closed, avoiding the slot-exhaustion issues that required
	// explicit flushing with /dev/tpm0.
	tpmDevicePath = "/dev/tpmrm0"

	// maxAttestResponseSize limits the attestation HTTP response body.
	maxAttestResponseSize = 1 << 20 // 1 MiB

	// httpRetries is the number of retry attempts for transient HTTP errors.
	httpRetries = 5
)

// AttestResult holds the output of a successful TPM attestation.
type AttestResult struct {
	// Token is the bootstrap token obtained from the attestation server.
	Token string
	// CACert is the PEM-encoded cluster CA certificate returned by the
	// server. May be empty if the server did not include it.
	CACert string
}

// attestRequest is the JSON body sent to the attestation server.
type attestRequest struct {
	EKPub  []byte `json:"ekPub"`  // TPM2B_PUBLIC (marshalled)
	SRKPub []byte `json:"srkPub"` // TPM2B_PUBLIC (marshalled)
}

// attestResponse is the JSON body returned by the attestation server.
type attestResponse struct {
	CredentialBlob  []byte `json:"credentialBlob"`      // TPM2B(idObject)
	EncryptedSecret []byte `json:"encryptedSecret"`     // TPM2B(encSecret)
	EncryptedToken  []byte `json:"encryptedToken"`      // AES-256-GCM ciphertext
	GCMNonce        []byte `json:"gcmNonce"`            // AES-GCM nonce
	ClusterCA       string `json:"clusterCA,omitempty"` // PEM-encoded CA certificate
}

type tpmAttest struct {
	log         *slog.Logger
	attestURL   string
	machineName string
	result      *AttestResult

	// openTPM is an override for tests. When nil, the real TPM device is opened.
	openTPM func() (transport.TPMCloser, error)
}

// TPMAttest returns a task that performs TPM attestation against a metalman
// serve-pxe instance to obtain a bootstrap token. The attestation uses the
// google/go-tpm library to interact with the TPM device directly via
// /dev/tpmrm0, eliminating the need for tpm2-tools or Python.
//
// The result pointer is populated on success so that subsequent phases can
// use the retrieved token.
func TPMAttest(log *slog.Logger, attestURL, machineName string, result *AttestResult) phases.Task {
	return &tpmAttest{
		log:         log,
		attestURL:   attestURL,
		machineName: machineName,
		result:      result,
	}
}

func (t *tpmAttest) Name() string { return "tpm-attest" }

func (t *tpmAttest) Do(ctx context.Context) error {
	t.log.Info("running TPM attestation",
		slog.String("url", t.attestURL),
		slog.String("machine", t.machineName),
	)

	token, clusterCA, err := t.doAttest(ctx)
	if err != nil {
		return fmt.Errorf("attestation failed: %w", err)
	}

	t.result.Token = token
	t.result.CACert = clusterCA

	t.log.Info("TPM attestation succeeded")

	return nil
}

// doAttest performs the full TPM attestation flow:
//  1. Open the TPM device.
//  2. Create the EK (Endorsement Key) using the TCG RSA EK template.
//  3. Create the SRK (Storage Root Key) using the TCG RSA SRK template.
//  4. Read the EK and SRK public keys in TPM2B_PUBLIC format.
//  5. POST the public keys to the attestation server.
//  6. Recover the AES key via TPM2_ActivateCredential (using a PolicySecret
//     session on the EK, since the EK requires endorsement-hierarchy auth).
//  7. Decrypt the bootstrap token with the recovered AES-256-GCM key.
func (t *tpmAttest) doAttest(ctx context.Context) (token, clusterCA string, err error) {
	// Open the TPM.
	openFn := t.openTPM
	if openFn == nil {
		openFn = func() (transport.TPMCloser, error) {
			return linuxtpm.Open(tpmDevicePath)
		}
	}

	tpmDev, err := openFn()
	if err != nil {
		return "", "", fmt.Errorf("open TPM %s: %w", tpmDevicePath, err)
	}
	defer tpmDev.Close() //nolint:errcheck

	// Create the EK using the standard RSA EK template.
	t.log.Info("creating EK")

	ekResp, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.TPMRHEndorsement,
		InPublic:      tpm2.New2B(tpm2.RSAEKTemplate),
	}.Execute(tpmDev)
	if err != nil {
		return "", "", fmt.Errorf("CreatePrimary(EK): %w", err)
	}
	defer flushHandle(tpmDev, ekResp.ObjectHandle) //nolint:errcheck

	t.log.Info("EK created")

	// Create the SRK using the standard RSA SRK template.
	t.log.Info("creating SRK")

	srkResp, err := tpm2.CreatePrimary{
		PrimaryHandle: tpm2.AuthHandle{
			Handle: tpm2.TPMRHOwner,
			Auth:   tpm2.PasswordAuth(nil),
		},
		InPublic: tpm2.New2B(tpm2.RSASRKTemplate),
	}.Execute(tpmDev)
	if err != nil {
		return "", "", fmt.Errorf("CreatePrimary(SRK): %w", err)
	}
	defer flushHandle(tpmDev, srkResp.ObjectHandle) //nolint:errcheck

	t.log.Info("SRK created")

	// Read the EK and SRK public keys in TPM2B_PUBLIC wire format, which
	// is what the attestation server expects.
	ekPubBytes := tpm2.Marshal(ekResp.OutPublic)
	srkPubBytes := tpm2.Marshal(srkResp.OutPublic)

	// POST the public keys to the attestation server.
	t.log.Info("sending attestation request", slog.String("url", t.attestURL+"/attest"))

	resp, err := t.postAttest(ctx, ekPubBytes, srkPubBytes)
	if err != nil {
		return "", "", err
	}

	// Parse the TPM2B-wrapped credential blob and encrypted secret.
	// The server sends them as TPM2B: [2-byte big-endian length][data].
	idObject, err := parseTPM2B(resp.CredentialBlob, "credentialBlob")
	if err != nil {
		return "", "", err
	}

	encSecret, err := parseTPM2B(resp.EncryptedSecret, "encryptedSecret")
	if err != nil {
		return "", "", err
	}

	// Recover the AES key via ActivateCredential.
	//
	// The EK uses the standard EK policy: PolicySecret(RH_ENDORSEMENT).
	// We must create a policy session satisfying that policy to authorize
	// use of the EK as the credentialed-key handle.
	t.log.Info("activating credential")

	aesKey, err := activateCredentialWithPolicy(tpmDev, srkResp.ObjectHandle, srkResp.Name, ekResp.ObjectHandle, ekResp.Name, idObject, encSecret)
	if err != nil {
		return "", "", fmt.Errorf("ActivateCredential: %w", err)
	}

	t.log.Info("credential activated")

	// Decrypt the bootstrap token with the recovered AES-256-GCM key.
	plaintext, err := decryptAESGCM(aesKey, resp.GCMNonce, resp.EncryptedToken)
	if err != nil {
		return "", "", fmt.Errorf("decrypt token: %w", err)
	}

	return string(plaintext), resp.ClusterCA, nil
}

// activateCredentialWithPolicy runs TPM2_ActivateCredential with a policy
// session that satisfies PolicySecret(RH_ENDORSEMENT), as required by the
// standard RSA EK template.
func activateCredentialWithPolicy(
	tpmDev transport.TPM,
	srkHandle tpm2.TPMHandle, srkName tpm2.TPM2BName,
	ekHandle tpm2.TPMHandle, ekName tpm2.TPM2BName,
	idObject, encSecret []byte,
) ([]byte, error) {
	// Start a policy session.
	sess, closer, err := tpm2.PolicySession(tpmDev, tpm2.TPMAlgSHA256, 16)
	if err != nil {
		return nil, fmt.Errorf("PolicySession: %w", err)
	}
	defer closer() //nolint:errcheck

	// Satisfy PolicySecret(RH_ENDORSEMENT).
	_, err = tpm2.PolicySecret{
		AuthHandle: tpm2.AuthHandle{
			Handle: tpm2.TPMRHEndorsement,
			Auth:   tpm2.PasswordAuth(nil),
		},
		PolicySession: sess.Handle(),
	}.Execute(tpmDev)
	if err != nil {
		return nil, fmt.Errorf("PolicySecret(endorsement): %w", err)
	}

	// ActivateCredential: the SRK is the activate handle (the object
	// whose Name was used in CreateCredential), and the EK is the key
	// handle (the key that protects the credential). The EK auth is
	// provided by the policy session we just set up.
	acResp, err := tpm2.ActivateCredential{
		ActivateHandle: tpm2.AuthHandle{
			Handle: srkHandle,
			Name:   srkName,
			Auth:   tpm2.PasswordAuth(nil),
		},
		KeyHandle: tpm2.AuthHandle{
			Handle: ekHandle,
			Name:   ekName,
			Auth:   sess,
		},
		CredentialBlob: tpm2.TPM2BIDObject{
			Buffer: idObject,
		},
		Secret: tpm2.TPM2BEncryptedSecret{
			Buffer: encSecret,
		},
	}.Execute(tpmDev)
	if err != nil {
		return nil, err
	}

	return acResp.CertInfo.Buffer, nil
}

// postAttest sends the EK and SRK public keys to the attestation server
// and returns the parsed response. It retries on transient network errors
// with exponential backoff.
func (t *tpmAttest) postAttest(ctx context.Context, ekPub, srkPub []byte) (*attestResponse, error) {
	reqBody, err := json.Marshal(attestRequest{
		EKPub:  ekPub,
		SRKPub: srkPub,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal attest request: %w", err)
	}

	url := t.attestURL + "/attest"

	var resp *http.Response

	for attempt := range httpRetries {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
		if err != nil {
			return nil, fmt.Errorf("create HTTP request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err = http.DefaultClient.Do(req)
		if err != nil {
			if attempt == httpRetries-1 {
				return nil, fmt.Errorf("POST %s: %w", url, err)
			}

			delay := expBackoff(attempt)
			t.log.Warn("connection error, retrying",
				slog.String("url", url),
				slog.String("err", err.Error()),
				slog.Duration("delay", delay),
			)

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}

			continue
		}

		break
	}

	defer resp.Body.Close() //nolint:errcheck

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAttestResponseSize))
	if err != nil {
		return nil, fmt.Errorf("read attest response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("attest server returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var attestResp attestResponse
	if err := json.Unmarshal(body, &attestResp); err != nil {
		return nil, fmt.Errorf("parse attest response: %w", err)
	}

	return &attestResp, nil
}

// parseTPM2B strips the 2-byte big-endian length prefix from a TPM2B-encoded
// byte slice and returns the inner data. The server wraps the credential blob
// and encrypted secret in this format for the wire protocol.
func parseTPM2B(data []byte, fieldName string) ([]byte, error) {
	if len(data) < 2 {
		return nil, fmt.Errorf("%s: too short for TPM2B (len=%d)", fieldName, len(data))
	}

	size := binary.BigEndian.Uint16(data[:2])
	inner := data[2:]

	if int(size) > len(inner) {
		return nil, fmt.Errorf("%s: TPM2B size %d exceeds data length %d", fieldName, size, len(inner))
	}

	return inner[:size], nil
}

// decryptAESGCM decrypts AES-256-GCM ciphertext with the given key and nonce.
func decryptAESGCM(key, nonce, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("GCM: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("GCM decrypt: %w", err)
	}

	return plaintext, nil
}

// flushHandle flushes a transient TPM handle. Errors are intentionally
// ignored since this is best-effort cleanup.
func flushHandle(tpmDev transport.TPM, handle tpm2.TPMHandle) error {
	_, err := tpm2.FlushContext{FlushHandle: handle}.Execute(tpmDev)

	return err
}

// expBackoff returns an exponential backoff duration capped at 30 seconds.
func expBackoff(attempt int) time.Duration {
	secs := math.Min(math.Pow(2, float64(attempt)), 30)

	return time.Duration(secs * float64(time.Second))
}

// CleanupAttestArtifacts is a no-op now that the attestation is performed
// entirely in Go. The TPM handles are cleaned up when the device file
// descriptor is closed, and no scripts or config files are written to disk.
func CleanupAttestArtifacts() {}

// applyAttestation is a task that conditionally runs TPM attestation and
// applies the result to the node-start goal state. When attestation is
// not configured (nil config or empty URL), the task is a no-op.
type applyAttestation struct {
	log         *slog.Logger
	attest      *provision.AgentAttestConfig
	machineName string
	goalState   *goalstates.NodeStart
}

// ApplyAttestation returns a task that performs TPM attestation when the
// provided config enables it. If attest is nil or its URL is empty, the
// task succeeds immediately without contacting the TPM or any server.
//
// On successful attestation the bootstrap token (and, if present, the
// cluster CA certificate) in goalState.Kubelet are replaced with the
// values obtained from the attestation server.
func ApplyAttestation(log *slog.Logger, attest *provision.AgentAttestConfig, machineName string, goalState *goalstates.NodeStart) phases.Task {
	return &applyAttestation{
		log:         log,
		attest:      attest,
		machineName: machineName,
		goalState:   goalState,
	}
}

func (a *applyAttestation) Name() string { return "apply-attestation" }

func (a *applyAttestation) Do(ctx context.Context) error {
	if a.attest == nil || a.attest.URL == "" {
		a.log.Info("attestation not configured, skipping")
		return nil
	}

	var result AttestResult

	attestTask := TPMAttest(a.log, a.attest.URL, a.machineName, &result)
	if err := attestTask.Do(ctx); err != nil {
		return fmt.Errorf("tpm-attest: %w", err)
	}

	a.goalState.Kubelet.BootstrapToken = result.Token

	if result.CACert != "" {
		a.goalState.Kubelet.CACertData = []byte(result.CACert)
	}

	return nil
}
