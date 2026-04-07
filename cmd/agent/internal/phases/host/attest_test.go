package host

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-tpm-tools/simulator"
	"github.com/google/go-tpm/tpm2"
	"github.com/google/go-tpm/tpm2/transport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/project-unbounded/unbounded-kube/internal/helpers"
)

// TestParseTPM2B tests the TPM2B wire-format parser.
func TestParseTPM2B(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		data    []byte
		want    []byte
		wantErr string
	}{
		{
			name: "valid",
			data: append([]byte{0x00, 0x03}, []byte("abc")...),
			want: []byte("abc"),
		},
		{
			name: "empty payload",
			data: []byte{0x00, 0x00},
			want: []byte{},
		},
		{
			name: "extra trailing bytes ignored",
			data: append([]byte{0x00, 0x02}, []byte("abcd")...),
			want: []byte("ab"),
		},
		{
			name:    "too short",
			data:    []byte{0x00},
			wantErr: "too short",
		},
		{
			name:    "nil",
			data:    nil,
			wantErr: "too short",
		},
		{
			name:    "size exceeds data",
			data:    []byte{0x00, 0x05, 0x01, 0x02},
			wantErr: "exceeds data length",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseTPM2B(tt.data, "test")
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)

				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestDecryptAESGCM tests AES-256-GCM decryption.
func TestDecryptAESGCM(t *testing.T) {
	t.Parallel()

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)

	block, err := aes.NewCipher(key)
	require.NoError(t, err)

	gcm, err := cipher.NewGCM(block)
	require.NoError(t, err)

	nonce := make([]byte, gcm.NonceSize())
	_, err = io.ReadFull(rand.Reader, nonce)
	require.NoError(t, err)

	plaintext := []byte("hello-bootstrap-token")
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	// Successful decryption.
	got, err := decryptAESGCM(key, nonce, ciphertext)
	require.NoError(t, err)
	assert.Equal(t, plaintext, got)

	// Wrong key fails.
	badKey := make([]byte, 32)
	_, err = rand.Read(badKey)
	require.NoError(t, err)

	_, err = decryptAESGCM(badKey, nonce, ciphertext)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GCM decrypt")

	// Tampered ciphertext fails.
	tampered := make([]byte, len(ciphertext))
	copy(tampered, ciphertext)
	tampered[0] ^= 0xFF

	_, err = decryptAESGCM(key, nonce, tampered)
	require.Error(t, err)
}

// TestExpBackoff verifies exponential backoff timing.
func TestExpBackoff(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 1*time.Second, expBackoff(0))
	assert.Equal(t, 2*time.Second, expBackoff(1))
	assert.Equal(t, 4*time.Second, expBackoff(2))
	assert.Equal(t, 8*time.Second, expBackoff(3))
	assert.Equal(t, 16*time.Second, expBackoff(4))
	// Capped at 30s.
	assert.Equal(t, 30*time.Second, expBackoff(5))
	assert.Equal(t, 30*time.Second, expBackoff(10))
}

// nonClosingTPM wraps a transport.TPM to suppress Close calls. This lets
// the test control the simulator lifetime while doAttest() calls Close()
// on the handle it receives.
type nonClosingTPM struct {
	transport.TPM
}

func (n *nonClosingTPM) Close() error { return nil }

// openSimulatorForE2E opens a software TPM simulator for an end-to-end
// test. The returned handle's Close() is a no-op so that doAttest() can
// call it without tearing down the simulator mid-test.
func openSimulatorForE2E(t *testing.T) transport.TPMCloser {
	t.Helper()

	if !helpers.CgoEnabled {
		t.Skipf("skipping because the TPU simulator requires CGO, which is disabled")
	}

	sim, err := simulator.Get()
	require.NoError(t, err)

	tpmDev := transport.FromReadWriteCloser(sim)
	t.Cleanup(func() { tpmDev.Close() })

	return &nonClosingTPM{TPM: tpmDev}
}

// fakeAttestServer creates an httptest.Server that implements the server-side
// attestation protocol (CreateCredential + AES-GCM encryption) and returns
// the given token and CA cert on success.
func fakeAttestServer(t *testing.T, token, clusterCA string) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/attest" {
			http.NotFound(w, r)
			return
		}

		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req attestRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}

		// Decode the EK and SRK public keys sent by the client.
		ekTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](req.EKPub)
		if err != nil {
			http.Error(w, "invalid EK pub", http.StatusBadRequest)
			return
		}

		ekTPMPub, err := ekTPM2B.Contents()
		if err != nil {
			http.Error(w, "invalid EK pub contents", http.StatusBadRequest)
			return
		}

		srkTPM2B, err := tpm2.Unmarshal[tpm2.TPM2BPublic, *tpm2.TPM2BPublic](req.SRKPub)
		if err != nil {
			http.Error(w, "invalid SRK pub", http.StatusBadRequest)
			return
		}

		srkTPMPub, err := srkTPM2B.Contents()
		if err != nil {
			http.Error(w, "invalid SRK pub contents", http.StatusBadRequest)
			return
		}

		srkName, err := tpm2.ObjectName(srkTPMPub)
		if err != nil {
			http.Error(w, "SRK name", http.StatusInternalServerError)
			return
		}

		ekEncKey, err := tpm2.ImportEncapsulationKey(ekTPMPub)
		if err != nil {
			http.Error(w, "import EK", http.StatusInternalServerError)
			return
		}

		// Generate random AES-256 key and wrap via CreateCredential.
		aesKey := make([]byte, 32)
		if _, err := rand.Read(aesKey); err != nil {
			http.Error(w, "rand", http.StatusInternalServerError)
			return
		}

		idObject, encSecret, err := tpm2.CreateCredential(rand.Reader, ekEncKey, srkName.Buffer, aesKey)
		if err != nil {
			http.Error(w, "CreateCredential", http.StatusInternalServerError)
			return
		}

		// Wrap in TPM2B format (2-byte big-endian length prefix).
		credBlob := make([]byte, 2+len(idObject))
		binary.BigEndian.PutUint16(credBlob[:2], uint16(len(idObject)))
		copy(credBlob[2:], idObject)

		encSecretWrapped := make([]byte, 2+len(encSecret))
		binary.BigEndian.PutUint16(encSecretWrapped[:2], uint16(len(encSecret)))
		copy(encSecretWrapped[2:], encSecret)

		// Encrypt the bootstrap token with the AES key.
		block, err := aes.NewCipher(aesKey)
		if err != nil {
			http.Error(w, "aes", http.StatusInternalServerError)
			return
		}

		gcm, err := cipher.NewGCM(block)
		if err != nil {
			http.Error(w, "gcm", http.StatusInternalServerError)
			return
		}

		gcmNonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, gcmNonce); err != nil {
			http.Error(w, "nonce", http.StatusInternalServerError)
			return
		}

		encryptedToken := gcm.Seal(nil, gcmNonce, []byte(token), nil)

		resp := attestResponse{
			CredentialBlob:  credBlob,
			EncryptedSecret: encSecretWrapped,
			EncryptedToken:  encryptedToken,
			GCMNonce:        gcmNonce,
			ClusterCA:       clusterCA,
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck
	}))

	t.Cleanup(server.Close)

	return server
}

// TestTPMAttestE2E runs the full attestation flow against a simulated TPM
// and a test HTTP server implementing the server-side attestation protocol.
func TestTPMAttestE2E(t *testing.T) {
	sim := openSimulatorForE2E(t)

	expectedToken := "test-bootstrap-token-12345"
	clusterCA := "-----BEGIN CERTIFICATE-----\nfake-ca-cert\n-----END CERTIFICATE-----\n"

	server := fakeAttestServer(t, expectedToken, clusterCA)

	task := &tpmAttest{
		log:         slog.Default(),
		attestURL:   server.URL,
		machineName: "test-node",
		result:      &AttestResult{},
		openTPM: func() (transport.TPMCloser, error) {
			return sim, nil
		},
	}

	err := task.Do(t.Context())
	require.NoError(t, err)

	assert.Equal(t, expectedToken, task.result.Token)
	assert.Equal(t, clusterCA, task.result.CACert)
}

// TestTPMAttestNoClusterCA verifies that attestation succeeds when the
// server does not return a cluster CA certificate.
func TestTPMAttestNoClusterCA(t *testing.T) {
	sim := openSimulatorForE2E(t)

	expectedToken := "token-no-ca"
	server := fakeAttestServer(t, expectedToken, "")

	task := &tpmAttest{
		log:         slog.Default(),
		attestURL:   server.URL,
		machineName: "test-node",
		result:      &AttestResult{},
		openTPM: func() (transport.TPMCloser, error) {
			return sim, nil
		},
	}

	err := task.Do(t.Context())
	require.NoError(t, err)

	assert.Equal(t, expectedToken, task.result.Token)
	assert.Empty(t, task.result.CACert)
}

// TestTPMAttestServerError verifies that attestation fails gracefully
// when the server returns an error.
func TestTPMAttestServerError(t *testing.T) {
	sim := openSimulatorForE2E(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "node not found", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	task := &tpmAttest{
		log:         slog.Default(),
		attestURL:   server.URL,
		machineName: "unknown-node",
		result:      &AttestResult{},
		openTPM: func() (transport.TPMCloser, error) {
			return sim, nil
		},
	}

	err := task.Do(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HTTP 404")
}

// TestTPMAttestBadResponse verifies that attestation fails when the server
// returns invalid JSON.
func TestTPMAttestBadResponse(t *testing.T) {
	sim := openSimulatorForE2E(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{invalid json`)) //nolint:errcheck
	}))
	t.Cleanup(server.Close)

	task := &tpmAttest{
		log:         slog.Default(),
		attestURL:   server.URL,
		machineName: "test-node",
		result:      &AttestResult{},
		openTPM: func() (transport.TPMCloser, error) {
			return sim, nil
		},
	}

	err := task.Do(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse attest response")
}

// TestTPMAttestName verifies the task name.
func TestTPMAttestName(t *testing.T) {
	t.Parallel()

	task := TPMAttest(slog.Default(), "http://example.com", "node-1", &AttestResult{})
	assert.Equal(t, "tpm-attest", task.Name())
}
