// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
	"github.com/Azure/unbounded/internal/provision"
)

const testAllocationUID = "6d3f0d6e-8d1a-4c1e-9f61-0d7b8f0b2a10"

// testCertificatePEM returns a self-signed certificate and its key, valid
// until notAfter.
func testCertificatePEM(t *testing.T, notAfter time.Time) ([]byte, []byte) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: "system:node:test-node"},
		NotBefore:    notAfter.Add(-2 * time.Hour),
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

// storeDaemonCertificate writes a daemon-controller certificate where the
// certificate store reads it.
func storeDaemonCertificate(t *testing.T, dir string, notAfter time.Time) {
	t.Helper()

	certPEM, keyPEM := testCertificatePEM(t, notAfter)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.crt"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.key"), keyPEM, 0o600))
}

// allocationConfig returns an allocation-backed config whose CA is a valid
// PEM certificate, as REST client construction requires.
func allocationConfig(t *testing.T, uid string) *provision.AgentConfig {
	t.Helper()

	caPEM, _ := testCertificatePEM(t, time.Now().Add(time.Hour))

	cfg := baseConfig()
	cfg.NodeName = "test-node"
	cfg.Cluster.CaCertBase64 = base64.StdEncoding.EncodeToString(caPEM)
	cfg.Kubelet.Configuration = map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + uid}

	return cfg
}

func allocationMachine(uid string) *v1alpha3.Machine {
	return &v1alpha3.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "test-machine", Generation: 3},
		Spec: v1alpha3.MachineSpec{
			AllocationRef: &v1alpha3.UIDReference{Name: "alloc-1", UID: types.UID(uid)},
		},
	}
}

func testAllocationLaunchOptions(t *testing.T) allocationLaunchOptions {
	t.Helper()

	return allocationLaunchOptions{
		credentialDir: t.TempDir(),
		newClient: func(*rest.Config, client.Options) (client.WithWatch, error) {
			t.Error("unexpected client construction")

			return nil, errors.New("unexpected client construction")
		},
		interval: time.Millisecond,
		timeout:  50 * time.Millisecond,
	}
}

// countingReader counts Machine reads and fails the first failures of them.
func countingReader(objs []client.Object, reads *atomic.Int32, failures []error) client.WithWatch {
	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				n := int(reads.Add(1))
				if n <= len(failures) && failures[n-1] != nil {
					return failures[n-1]
				}

				return c.Get(ctx, key, obj, opts...)
			},
			Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
				return errors.New("unexpected create")
			},
		}).
		Build()
}

// failingReader fails the test on any read.
type failingReader struct{ t *testing.T }

func (r failingReader) Get(context.Context, client.ObjectKey, client.Object, ...client.GetOption) error {
	r.t.Error("unexpected live read")

	return errors.New("unexpected live read")
}

func (r failingReader) List(context.Context, client.ObjectList, ...client.ListOption) error {
	r.t.Error("unexpected live list")

	return errors.New("unexpected live list")
}

func repeat(err error, n int) []error {
	out := make([]error, n)
	for i := range out {
		out[i] = err
	}

	return out
}

func TestAllocationUID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		configuration map[string]any
		wantUID       string
		wantBacked    bool
	}{
		{name: "prefix with uid", configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix + "abc"}, wantUID: "abc", wantBacked: true},
		{name: "prefix with empty uid", configuration: map[string]any{"providerID": v1alpha3.AllocationProviderIDPrefix}, wantBacked: true},
		{name: "another providerID", configuration: map[string]any{"providerID": "azure:///subscriptions/x/vm"}},
		{name: "prefix not at start", configuration: map[string]any{"providerID": "x" + v1alpha3.AllocationProviderIDPrefix + "abc"}},
		{name: "non-string providerID", configuration: map[string]any{"providerID": 42}},
		{name: "nil configuration"},
		{name: "no providerID", configuration: map[string]any{"maxPods": 110}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := baseConfig()
			cfg.Kubelet.Configuration = tc.configuration

			uid, backed := AllocationUID(cfg)
			assert.Equal(t, tc.wantUID, uid)
			assert.Equal(t, tc.wantBacked, backed)
		})
	}
}

func TestVerifyAllocationLaunch_NotAllocationBacked(t *testing.T) {
	t.Parallel()

	opts := testAllocationLaunchOptions(t)
	// A corrupt certificate refuses if the guard ever reads the directory.
	require.NoError(t, os.WriteFile(filepath.Join(opts.credentialDir, "client.crt"), []byte("corrupt"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(opts.credentialDir, "client.key"), []byte("corrupt"), 0o600))

	for _, cfg := range []*provision.AgentConfig{
		baseConfig(),
		func() *provision.AgentConfig {
			cfg := baseConfig()
			cfg.Kubelet.Configuration = map[string]any{"providerID": "azure:///subscriptions/x/vm"}

			return cfg
		}(),
	} {
		require.NoError(t, verifyAllocationLaunch(context.Background(), discardLogger(), cfg, opts))
		require.NoError(t, verifyAllocationMachine(context.Background(), discardLogger(), failingReader{t: t}, cfg, opts))
	}
}

func TestVerifyAllocationMachine(t *testing.T) {
	t.Parallel()

	unavailable := apierrors.NewServiceUnavailable("api server is down")
	deleting := allocationMachine(testAllocationUID)
	deleting.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	deleting.Finalizers = []string{"example.com/hold"}

	fenced := allocationMachine(testAllocationUID)
	fenced.Spec.Fenced = true

	unbound := allocationMachine(testAllocationUID)
	unbound.Spec.AllocationRef = nil

	foreign := allocationMachine(testAllocationUID)
	foreign.Spec.AllocationRef.UID = "9c8b7a69-5847-4362-a1b0-c9d8e7f6a5b4"

	for _, tc := range []struct {
		name      string
		machine   *v1alpha3.Machine
		failures  []error
		cancel    bool
		wantReads int32
		// wantMoreReads asks for more than wantReads reads instead of exactly.
		wantMoreReads bool
		wantAllowed   bool
		wantFinal     bool
		wantCause     error
	}{
		{name: "allow", machine: allocationMachine(testAllocationUID), wantReads: 1, wantAllowed: true},
		{name: "fenced", machine: fenced, wantReads: 1, wantFinal: true, wantCause: errAllocationMachineFenced},
		{name: "deleting", machine: deleting, wantReads: 1, wantFinal: true, wantCause: errAllocationMachineDeleting},
		{name: "allocationRef nil", machine: unbound, wantReads: 1, wantFinal: true, wantCause: errAllocationMachineForeign},
		{name: "uid mismatch", machine: foreign, wantReads: 1, wantFinal: true, wantCause: errAllocationMachineForeign},
		{name: "not found", wantReads: 1, wantFinal: true},
		{
			name:      "unauthorized",
			machine:   allocationMachine(testAllocationUID),
			failures:  []error{apierrors.NewUnauthorized("certificate rejected")},
			wantReads: 1,
			wantFinal: true,
		},
		{
			name:      "forbidden",
			machine:   allocationMachine(testAllocationUID),
			failures:  []error{apierrors.NewForbidden(schema.GroupResource{Group: "unbounded-cloud.io", Resource: "machines"}, "test-machine", errors.New("denied"))},
			wantReads: 1,
			wantFinal: true,
		},
		{
			name:        "transient then allow",
			machine:     allocationMachine(testAllocationUID),
			failures:    []error{unavailable},
			wantReads:   2,
			wantAllowed: true,
		},
		{
			name:          "transient until the budget",
			machine:       allocationMachine(testAllocationUID),
			failures:      repeat(unavailable, 100000),
			wantReads:     1,
			wantMoreReads: true,
			wantCause:     unavailable,
		},
		{
			name:      "canceled context",
			machine:   allocationMachine(testAllocationUID),
			cancel:    true,
			wantReads: 0,
			wantCause: context.Canceled,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var objs []client.Object
			if tc.machine != nil {
				objs = append(objs, tc.machine)
			}

			var reads atomic.Int32

			reader := countingReader(objs, &reads, tc.failures)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tc.cancel {
				cancel()
			}

			err := verifyAllocationMachine(ctx, discardLogger(), reader, allocationConfig(t, testAllocationUID), testAllocationLaunchOptions(t))

			if tc.wantMoreReads {
				assert.Greater(t, reads.Load(), tc.wantReads)
			} else {
				assert.Equal(t, tc.wantReads, reads.Load())
			}

			if tc.wantAllowed {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrAllocationLaunchRefused)
			assert.Equal(t, !tc.wantFinal, errors.Is(err, errAllocationLaunchUnconfirmed), "unconfirmed: %v", err)
			assert.Equal(t, tc.wantFinal, IsFinalAllocationLaunchRefusal(err), "final: %v", err)

			if tc.wantCause != nil {
				require.ErrorIs(t, err, tc.wantCause)
			}
		})
	}
}

func TestVerifyAllocationMachine_MalformedIdentity(t *testing.T) {
	t.Parallel()

	err := verifyAllocationMachine(context.Background(), discardLogger(), failingReader{t: t}, allocationConfig(t, ""), testAllocationLaunchOptions(t))
	require.ErrorIs(t, err, ErrAllocationLaunchRefused)
	require.ErrorIs(t, err, errAllocationIdentityMalformed)
}

// tokenClient counts SelfSubjectReview creates and fails the first failures
// of them. It fails the test on any Machine read.
func tokenClient(t *testing.T, creates *atomic.Int32, failures []error) client.WithWatch {
	t.Helper()

	return fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
				t.Error("unexpected Machine read with the bootstrap token")

				return errors.New("unexpected read")
			},
			Create: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.CreateOption) error {
				if _, ok := obj.(*authenticationv1.SelfSubjectReview); !ok {
					t.Errorf("unexpected create of %T", obj)
				}

				n := int(creates.Add(1))
				if n <= len(failures) {
					return failures[n-1]
				}

				return nil
			},
		}).
		Build()
}

func TestVerifyAllocationLaunch_TokenCheck(t *testing.T) {
	t.Parallel()

	unavailable := apierrors.NewServiceUnavailable("api server is down")

	for _, tc := range []struct {
		name          string
		failures      []error
		wantCreates   int32
		wantMoreCalls bool
		wantAllowed   bool
		wantFinal     bool
	}{
		{name: "token accepted", wantCreates: 1, wantAllowed: true},
		{name: "unauthorized", failures: []error{apierrors.NewUnauthorized("token revoked")}, wantCreates: 1, wantFinal: true},
		{
			name:        "forbidden",
			failures:    []error{apierrors.NewForbidden(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, "", errors.New("denied"))},
			wantCreates: 1,
			wantFinal:   true,
		},
		{name: "transient then allow", failures: []error{unavailable}, wantCreates: 2, wantAllowed: true},
		{name: "unavailable until the budget", failures: repeat(unavailable, 100000), wantCreates: 1, wantMoreCalls: true},
		{
			name:          "review endpoint not served",
			failures:      repeat(apierrors.NewNotFound(schema.GroupResource{Group: "authentication.k8s.io", Resource: "selfsubjectreviews"}, ""), 100000),
			wantCreates:   1,
			wantMoreCalls: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				creates   atomic.Int32
				newClient atomic.Int32
			)

			opts := testAllocationLaunchOptions(t)
			opts.newClient = func(cfg *rest.Config, _ client.Options) (client.WithWatch, error) {
				newClient.Add(1)
				assert.Equal(t, "abc123.xyz789", cfg.BearerToken)

				return tokenClient(t, &creates, tc.failures), nil
			}

			err := verifyAllocationLaunch(context.Background(), discardLogger(), allocationConfig(t, testAllocationUID), opts)
			// Every read selects its credential and builds its client again.
			assert.Equal(t, creates.Load(), newClient.Load())

			if tc.wantMoreCalls {
				assert.Greater(t, creates.Load(), tc.wantCreates)
			} else {
				assert.Equal(t, tc.wantCreates, creates.Load())
			}

			if tc.wantAllowed {
				require.NoError(t, err)

				return
			}

			require.ErrorIs(t, err, ErrAllocationLaunchRefused)
			assert.Equal(t, !tc.wantFinal, errors.Is(err, errAllocationLaunchUnconfirmed), "unconfirmed: %v", err)
			assert.Equal(t, tc.wantFinal, IsFinalAllocationLaunchRefusal(err), "final: %v", err)

			if tc.failures != nil {
				require.ErrorIs(t, err, tc.failures[0])
			}
		})
	}
}

func TestVerifyAllocationLaunch_TokenCheckMisconfigured(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		mutate func(*provision.AgentConfig)
		want   string
	}{
		{name: "missing token", mutate: func(cfg *provision.AgentConfig) { cfg.Kubelet.Auth.BootstrapToken = "" }, want: "no bootstrap token"},
		{name: "missing CA", mutate: func(cfg *provision.AgentConfig) { cfg.Cluster.CaCertBase64 = "" }, want: "no CA certificate"},
		{name: "missing API server", mutate: func(cfg *provision.AgentConfig) { cfg.Kubelet.ApiServer = "" }, want: "no API server URL"},
		{name: "malformed identity", mutate: func(cfg *provision.AgentConfig) {
			cfg.Kubelet.Configuration["providerID"] = v1alpha3.AllocationProviderIDPrefix
		}, want: "names no allocation UID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cfg := allocationConfig(t, testAllocationUID)
			tc.mutate(cfg)

			// The default options fail the test on client construction.
			err := verifyAllocationLaunch(context.Background(), discardLogger(), cfg, testAllocationLaunchOptions(t))
			require.ErrorIs(t, err, ErrAllocationLaunchRefused)
			require.ErrorContains(t, err, tc.want)
			assert.NotErrorIs(t, err, errAllocationLaunchUnconfirmed)
			assert.True(t, IsFinalAllocationLaunchRefusal(err), "final: %v", err)
		})
	}
}

func TestVerifyAllocationLaunch_CredentialSelection(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name          string
		store         func(t *testing.T, dir string)
		wantCert      bool
		wantRefused   bool
		wantNoClients bool
	}{
		{
			name:     "stored valid certificate reads the Machine",
			store:    func(t *testing.T, dir string) { storeDaemonCertificate(t, dir, time.Now().Add(time.Hour)) },
			wantCert: true,
		},
		{
			name:  "expired certificate uses the token",
			store: func(t *testing.T, dir string) { storeDaemonCertificate(t, dir, time.Now().Add(-time.Minute)) },
		},
		{
			name:  "empty credential directory uses the token",
			store: func(*testing.T, string) {},
		},
		{
			name: "corrupt certificate refuses",
			store: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, "client.crt"), []byte("corrupt"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "client.key"), []byte("corrupt"), 0o600))
			},
			wantRefused:   true,
			wantNoClients: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				clients atomic.Int32
				reads   atomic.Int32
				creates atomic.Int32
			)

			opts := testAllocationLaunchOptions(t)
			tc.store(t, opts.credentialDir)

			opts.newClient = func(cfg *rest.Config, _ client.Options) (client.WithWatch, error) {
				clients.Add(1)

				if cfg.BearerToken == "" {
					assert.NotNil(t, cfg.Transport, "certificate client has no transport")

					return countingReader([]client.Object{allocationMachine(testAllocationUID)}, &reads, nil), nil
				}

				return tokenClient(t, &creates, nil), nil
			}

			err := verifyAllocationLaunch(context.Background(), discardLogger(), allocationConfig(t, testAllocationUID), opts)

			if tc.wantNoClients {
				assert.Equal(t, int32(0), clients.Load())
			} else {
				assert.Equal(t, int32(1), clients.Load())
			}

			if tc.wantRefused {
				require.ErrorIs(t, err, ErrAllocationLaunchRefused)
				require.ErrorContains(t, err, "load stored daemon controller certificate")

				return
			}

			require.NoError(t, err)

			if tc.wantCert {
				assert.Equal(t, int32(1), reads.Load())
				assert.Equal(t, int32(0), creates.Load(), "the certificate path must not create a SelfSubjectReview")
			} else {
				assert.Equal(t, int32(0), reads.Load())
				assert.Equal(t, int32(1), creates.Load())
			}
		})
	}
}

// TestVerifyAllocationLaunch_CertificateDoesNotFallBack pins that a rejected
// certificate is final even while the bootstrap token would still be accepted.
func TestVerifyAllocationLaunch_CertificateDoesNotFallBack(t *testing.T) {
	t.Parallel()

	var (
		reads   atomic.Int32
		creates atomic.Int32
	)

	opts := testAllocationLaunchOptions(t)
	storeDaemonCertificate(t, opts.credentialDir, time.Now().Add(time.Hour))
	opts.newClient = func(cfg *rest.Config, _ client.Options) (client.WithWatch, error) {
		if cfg.BearerToken != "" {
			return tokenClient(t, &creates, nil), nil
		}

		return countingReader(nil, &reads, []error{apierrors.NewUnauthorized("certificate rejected")}), nil
	}

	err := verifyAllocationLaunch(context.Background(), discardLogger(), allocationConfig(t, testAllocationUID), opts)
	require.ErrorIs(t, err, ErrAllocationLaunchRefused)
	assert.True(t, IsFinalAllocationLaunchRefusal(err), "a certificate still valid after the read is final: %v", err)
	assert.Equal(t, int32(1), reads.Load())
	assert.Equal(t, int32(0), creates.Load())
}

// TestVerifyAllocationLaunch_CredentialReselected pins that every read selects
// its credential again, so the stored certificate can change between reads or
// during one.
func TestVerifyAllocationLaunch_CredentialReselected(t *testing.T) {
	t.Parallel()

	valid := func(t *testing.T, dir string) { storeDaemonCertificate(t, dir, time.Now().Add(time.Hour)) }
	expired := func(t *testing.T, dir string) { storeDaemonCertificate(t, dir, time.Now().Add(-time.Minute)) }
	unavailable := apierrors.NewServiceUnavailable("api server is down")
	unauthorized := apierrors.NewUnauthorized("credentials rejected")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "unbounded-cloud.io", Resource: "machines"}, "test-machine", errors.New("denied"))

	// hook runs inside the n-th read of its kind and returns the error that
	// read fails with, or nil to let it through.
	type hook func(t *testing.T, dir string, n int32) error

	for _, tc := range []struct {
		name           string
		stored         func(t *testing.T, dir string)
		onMachineRead  hook
		onCreate       hook
		wantAllowed    bool
		wantCredential string
		wantReads      int32
		wantCreates    int32
		wantClients    int32
	}{
		{
			// The API is down, the certificate expires, and the API recovers.
			name:   "expired between reads, token accepted",
			stored: valid,
			onMachineRead: func(t *testing.T, dir string, _ int32) error {
				expired(t, dir)

				return unavailable
			},
			wantAllowed:    true,
			wantCredential: allocationCredentialBootstrapToken,
			wantReads:      1,
			wantCreates:    1,
			wantClients:    2,
		},
		{
			name:   "expired between reads, token revoked",
			stored: valid,
			onMachineRead: func(t *testing.T, dir string, _ int32) error {
				expired(t, dir)

				return unavailable
			},
			onCreate:    func(*testing.T, string, int32) error { return unauthorized },
			wantReads:   1,
			wantCreates: 1,
			wantClients: 2,
		},
		{
			name:   "expired during the read",
			stored: valid,
			onMachineRead: func(t *testing.T, dir string, _ int32) error {
				expired(t, dir)

				return unauthorized
			},
			wantAllowed:    true,
			wantCredential: allocationCredentialBootstrapToken,
			wantReads:      1,
			wantCreates:    1,
			wantClients:    2,
		},
		{
			name:   "expired during the read, token revoked",
			stored: valid,
			onMachineRead: func(t *testing.T, dir string, _ int32) error {
				expired(t, dir)

				return unauthorized
			},
			onCreate:    func(*testing.T, string, int32) error { return unauthorized },
			wantReads:   1,
			wantCreates: 1,
			wantClients: 2,
		},
		{
			name:   "stored during the call",
			stored: func(*testing.T, string) {},
			onCreate: func(t *testing.T, dir string, _ int32) error {
				valid(t, dir)

				return unavailable
			},
			wantAllowed:    true,
			wantCredential: allocationCredentialDaemonCertificate,
			wantReads:      1,
			wantCreates:    1,
			wantClients:    2,
		},
		{
			name:          "forbidden from a valid certificate",
			stored:        valid,
			onMachineRead: func(*testing.T, string, int32) error { return forbidden },
			wantReads:     1,
			wantCreates:   0,
			wantClients:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				buf     bytes.Buffer
				clients atomic.Int32
				reads   atomic.Int32
				creates atomic.Int32
			)

			opts := testAllocationLaunchOptions(t)
			opts.timeout = 10 * time.Second
			tc.stored(t, opts.credentialDir)

			opts.newClient = func(cfg *rest.Config, _ client.Options) (client.WithWatch, error) {
				clients.Add(1)

				return fake.NewClientBuilder().
					WithScheme(newScheme()).
					WithObjects(allocationMachine(testAllocationUID)).
					WithInterceptorFuncs(interceptor.Funcs{
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts2 ...client.GetOption) error {
							n := reads.Add(1)

							if cfg.BearerToken != "" {
								t.Error("Machine read with the bootstrap token")
							}

							if tc.onMachineRead != nil {
								if err := tc.onMachineRead(t, opts.credentialDir, n); err != nil {
									return err
								}
							}

							return c.Get(ctx, key, obj, opts2...)
						},
						Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
							n := creates.Add(1)

							if cfg.BearerToken == "" {
								t.Error("SelfSubjectReview created with the certificate")
							}

							if tc.onCreate != nil {
								return tc.onCreate(t, opts.credentialDir, n)
							}

							return nil
						},
					}).
					Build(), nil
			}

			log := slog.New(slog.NewJSONHandler(&buf, nil))
			err := verifyAllocationLaunch(context.Background(), log, allocationConfig(t, testAllocationUID), opts)

			assert.Equal(t, tc.wantReads, reads.Load(), "Machine reads")
			assert.Equal(t, tc.wantCreates, creates.Load(), "SelfSubjectReview creates")
			assert.Equal(t, tc.wantClients, clients.Load(), "client constructions")

			if !tc.wantAllowed {
				require.ErrorIs(t, err, ErrAllocationLaunchRefused)
				assert.True(t, IsFinalAllocationLaunchRefusal(err), "final: %v", err)

				return
			}

			require.NoError(t, err)

			var allowed map[string]any

			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var entry map[string]any
				require.NoError(t, json.Unmarshal([]byte(line), &entry))

				if entry["msg"] == "allocation launch allowed" {
					allowed = entry
				}
			}

			require.NotNil(t, allowed, "no allow line in %s", buf.String())
			assert.Equal(t, tc.wantCredential, allowed["credential"])
		})
	}
}

func TestVerifyAllocationLaunch_LogsOneLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		machine    *v1alpha3.Machine
		wantMsg    string
		wantCause  string
		wantLevel  string
		credential string
	}{
		{name: "allow", machine: allocationMachine(testAllocationUID), wantMsg: "allocation launch allowed", wantLevel: "INFO"},
		{name: "refuse", wantMsg: "allocation launch refused", wantCause: "not found", wantLevel: "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var (
				buf   bytes.Buffer
				reads atomic.Int32
			)

			log := slog.New(slog.NewJSONHandler(&buf, nil))

			var objs []client.Object
			if tc.machine != nil {
				objs = append(objs, tc.machine)
			}

			_ = verifyAllocationMachine(context.Background(), log, countingReader(objs, &reads, nil), allocationConfig(t, testAllocationUID), testAllocationLaunchOptions(t))

			lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
			require.Len(t, lines, 1)

			var entry map[string]any
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
			assert.Equal(t, tc.wantMsg, entry["msg"])
			assert.Equal(t, tc.wantLevel, entry["level"])
			assert.Equal(t, testAllocationUID, entry["allocation_uid"])
			assert.Equal(t, "test-machine", entry["machine"])
			assert.Equal(t, allocationCredentialDaemonCertificate, entry["credential"])
			assert.Contains(t, entry["cause"], tc.wantCause)
		})
	}
}

func setAllocationLaunchConfigPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent", "allocation-launch-config.json")
	previous := allocationLaunchConfigPath
	allocationLaunchConfigPath = path

	t.Cleanup(func() { allocationLaunchConfigPath = previous })

	return path
}

func TestAllocationLaunchConfig(t *testing.T) {
	t.Run("persist writes an allocation config", func(t *testing.T) {
		path := setAllocationLaunchConfigPath(t)
		cfg := allocationConfig(t, testAllocationUID)

		require.NoError(t, PersistAllocationLaunchConfig(cfg))

		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

		data, err := os.ReadFile(path)
		require.NoError(t, err)

		var written provision.AgentConfig
		require.NoError(t, json.Unmarshal(data, &written))
		assert.Equal(t, cfg.Kubelet.Configuration["providerID"], written.Kubelet.Configuration["providerID"])
		assert.Equal(t, cfg.Kubelet.Auth.BootstrapToken, written.Kubelet.Auth.BootstrapToken)
	})

	t.Run("persist writes nothing for another config", func(t *testing.T) {
		path := setAllocationLaunchConfigPath(t)

		require.NoError(t, PersistAllocationLaunchConfig(baseConfig()))

		_, err := os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	})

	t.Run("load a missing record", func(t *testing.T) {
		setAllocationLaunchConfigPath(t)

		cfg, found, err := LoadAllocationLaunchConfig()
		require.NoError(t, err)
		assert.False(t, found)
		assert.Nil(t, cfg)
	})

	t.Run("load a written record", func(t *testing.T) {
		setAllocationLaunchConfigPath(t)
		want := allocationConfig(t, testAllocationUID)
		require.NoError(t, PersistAllocationLaunchConfig(want))

		cfg, found, err := LoadAllocationLaunchConfig()
		require.NoError(t, err)
		require.True(t, found)
		assert.Equal(t, "test-machine", cfg.MachineName)
		assert.Equal(t, "test-node", cfg.NodeName)

		uid, backed := AllocationUID(cfg)
		assert.True(t, backed)
		assert.Equal(t, testAllocationUID, uid)
	})

	t.Run("load a corrupt record", func(t *testing.T) {
		path := setAllocationLaunchConfigPath(t)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
		require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

		_, found, err := LoadAllocationLaunchConfig()
		require.ErrorContains(t, err, "decode allocation launch config")
		assert.False(t, found)
	})
}

func TestIsFinalAllocationLaunchRefusal(t *testing.T) {
	t.Parallel()

	refusal := func(cause error) error {
		return refuseAllocationLaunch(discardLogger(), allocationConfig(t, testAllocationUID), testAllocationUID, allocationCredentialDaemonCertificate, cause)
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	var reads atomic.Int32

	canceled := verifyAllocationMachine(canceledCtx, discardLogger(), countingReader(nil, &reads, nil),
		allocationConfig(t, testAllocationUID), testAllocationLaunchOptions(t))
	require.ErrorIs(t, canceled, context.Canceled)

	fenced := allocationMachine(testAllocationUID)
	fenced.Spec.Fenced = true
	fencedErr := verifyAllocationMachine(context.Background(), discardLogger(), countingReader([]client.Object{fenced}, &reads, nil),
		allocationConfig(t, testAllocationUID), testAllocationLaunchOptions(t))
	require.ErrorIs(t, fencedErr, errAllocationMachineFenced)

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("boom")},
		{name: "unconfirmed refusal", err: refusal(fmt.Errorf("%w: %w", errAllocationLaunchUnconfirmed, context.DeadlineExceeded))},
		{name: "canceled context refusal", err: canceled},
		{name: "bare refusal", err: fmt.Errorf("record: %w", ErrAllocationLaunchRefused), want: true},
		{name: "fenced from verifyAllocationMachine", err: fencedErr, want: true},
		{name: "fenced", err: refusal(errAllocationMachineFenced), want: true},
		{name: "deleting", err: refusal(errAllocationMachineDeleting), want: true},
		{name: "foreign", err: refusal(errAllocationMachineForeign), want: true},
		{name: "malformed identity", err: refusal(errAllocationIdentityMalformed), want: true},
		{name: "not found", err: refusal(apierrors.NewNotFound(schema.GroupResource{Resource: "machines"}, "test-machine")), want: true},
		{name: "unauthorized", err: refusal(apierrors.NewUnauthorized("rejected")), want: true},
		{name: "forbidden", err: refusal(apierrors.NewForbidden(schema.GroupResource{Resource: "machines"}, "test-machine", errors.New("denied"))), want: true},
		{name: "no bootstrap config", err: refusal(errors.New("build bootstrap rest config: applied config has no bootstrap token")), want: true},
		{name: "stored certificate unreadable", err: refusal(errors.New("load stored daemon controller certificate: bad PEM")), want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, IsFinalAllocationLaunchRefusal(tc.err))
		})
	}
}
