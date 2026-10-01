// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racer/wire"
)

func TestServingChainFreezesBeforeFirstRequest(t *testing.T) {
	for _, boundary := range []string{"handler", "tls"} {
		t.Run(boundary, func(t *testing.T) {
			f := newServingFixture(t)
			s := f.a.Server
			// The fixture issued its own test certificate. Replace dependencies
			// with unused direct constructions so no first request can mask a
			// missing serving-boundary freeze.
			b := s.Bootstrap
			i := b.Issuer
			s.Bootstrap = &Bootstrap{
				Client: b.Client, APIReader: b.APIReader, Config: b.Config,
				Issuer: &Issuer{APIReader: i.APIReader, Config: i.Config, Trust: i.Trust, CatalogGate: i.CatalogGate},
			}
			r := s.Replication
			s.Replication = &Replication{Client: r.Client, APIReader: r.APIReader, Config: r.Config, Publications: r.Publications, Trust: r.Trust, CatalogGate: r.CatalogGate}
			// Matching pre-start overrides are accepted and normalized once.
			for _, input := range []*Config{&s.Config, &s.Bootstrap.Config, &s.Bootstrap.Issuer.Config, &s.Replication.Config} {
				input.CertificateLifetime = 2 * time.Minute
				input.SnapshotMaxAge = 0
			}

			want := s.Config.effective()

			var handler http.Handler
			if boundary == "handler" {
				handler = s.Handler()
			} else {
				s.tlsConfigWithCertificate(f.ctx, func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &f.serverCertificate, nil })
			}
			// Mutate sequentially, before any request, without calling dependency
			// getters first: those calls would accidentally hide lazy freezing.
			s.Bootstrap.Config.DataplaneServiceAccount = "wrong-account"
			s.Bootstrap.Issuer.Config.Cluster = ""
			s.Bootstrap.Issuer.Config.CertificateLifetime = time.Second

			s.Replication.Config.ControllerServiceAccount = "wrong-controller"
			if handler == nil {
				handler = s.Handler()
			}

			for name, got := range map[string]Config{"server": s.config, "bootstrap": s.Bootstrap.runtimeConfig(), "issuer": s.Bootstrap.Issuer.runtimeConfig(), "replication": s.Replication.runtimeConfig()} {
				if got != want {
					t.Fatalf("%s did not freeze the same effective base before exposure", name)
				}
			}

			encoded, err := wire.EncodeBootstrapRequest(f.request)
			if err != nil {
				t.Fatal(err)
			}

			request := httptest.NewRequest(http.MethodPost, wire.BootstrapPath, bytes.NewReader(encoded))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+f.token)
			request.TLS = &tls.ConnectionState{HandshakeComplete: true}
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, request)

			if w.Code != http.StatusOK {
				t.Fatal("first request used post-exposure config", w.Code)
			}

			response := decodeIssuedResponse(t, w.Body.Bytes())

			leaf, err := x509.ParseCertificate(response.CertificateChain[0])
			if err != nil {
				t.Fatal(err)
			}

			if response.Cluster != want.Cluster || leaf.NotAfter.Sub(leaf.NotBefore) != want.CertificateLifetime {
				t.Fatal("first issuance ignored frozen identity/lifetime")
			}
		})
	}
}

func TestFrozenConfigUsedByRuntimeOperations(t *testing.T) {
	f := newServingFixture(t)
	a := f.a
	b := a.Server.Bootstrap
	request := httptest.NewRequest(http.MethodGet, wire.KeyringPath, nil)
	request.Header.Set("Authorization", "Bearer "+f.token)

	identity, err := b.Authenticate(f.ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	a.Replication.observe(f.ctx)
	handler := a.Server.Handler()
	// All components have crossed real operational boundaries, not just getters.
	for _, input := range []*Config{&a.Topology.Config, &a.Keyring.Config, &a.Replication.Config, &a.Server.Config, &b.Config, &b.Issuer.Config} {
		*input = Config{}
	}

	runKeys(t, a.Keyring)
	reconcileTopology(t, a.Topology, f.ctx)
	a.Replication.observe(f.ctx)

	if _, err := b.Authenticate(f.ctx, request); err != nil {
		t.Fatal("bootstrap reread config", err)
	}

	encoded, err := b.Issuer.Issue(f.ctx, identity, f.request)
	if err != nil {
		t.Fatal("issuer reread config", err)
	}

	if response, err := wire.DecodeBootstrapResponse(bytes.NewReader(encoded)); err != nil || response.Cluster != f.request.Cluster {
		t.Fatal("issued identity changed", err)
	}

	request.TLS = f.requestState(t)
	request.Header.Del("Authorization")

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, request)

	if w.Code != http.StatusOK {
		t.Fatal("serving/replication reread config", w.Code)
	}
}

func TestComponentConfigFreezesDefaultsAtFirstUse(t *testing.T) {
	a := Assemble(Config{}, nil, nil)
	// Test Server's own capture separately from its transitive dependency freeze.
	s := &Server{}
	// Assemble remains a pure scaffold, even with an invalid zero config.
	for name, component := range map[string]struct {
		input *Config
		get   func() Config
	}{
		"topology":    {&a.Topology.Config, a.Topology.runtimeConfig},
		"keyring":     {&a.Keyring.Config, a.Keyring.runtimeConfig},
		"replication": {&a.Replication.Config, a.Replication.runtimeConfig},
		"bootstrap":   {&a.Server.Bootstrap.Config, a.Server.Bootstrap.runtimeConfig},
		"issuer":      {&a.Server.Bootstrap.Issuer.Config, a.Server.Bootstrap.Issuer.runtimeConfig},
		"server":      {&s.Config, func() Config { s.initializeAdmission(); return s.config }},
	} {
		t.Run(name, func(t *testing.T) {
			*component.input = testConfig(t)
			component.input.CertificateLifetime = 0
			component.input.SnapshotMaxAge = 0
			component.input.PeerPort = 9443

			want := component.input.effective()
			if err := component.input.Validate(); err != nil {
				t.Fatal("zero optional lifetimes rejected", err)
			}

			if got := component.get(); got != want || got.CertificateLifetime != wire.CertificateLifetime || got.SnapshotMaxAge != 30*time.Second {
				t.Fatal("first use ignored pre-use inputs/defaults")
			}

			*component.input = Config{}

			var readers sync.WaitGroup
			for range 8 {
				readers.Go(func() {
					if component.get() != want {
						t.Error("runtime reread mutated construction inputs")
					}
				})
			}

			readers.Wait()
		})
	}
}

func TestDirectComponentConfigDefaults(t *testing.T) {
	for name, get := range map[string]func() Config{
		"topology":    (&TopologyReconciler{}).runtimeConfig,
		"keyring":     (&KeyringReconciler{}).runtimeConfig,
		"bootstrap":   (&Bootstrap{}).runtimeConfig,
		"issuer":      (&Issuer{}).runtimeConfig,
		"replication": (&Replication{}).runtimeConfig,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := get()
			if cfg.CertificateLifetime != wire.CertificateLifetime || cfg.SnapshotMaxAge != 30*time.Second {
				t.Fatal("direct zero-default semantics lost")
			}
		})
	}
}

func TestConfigDeploymentIdentityAndBounds(t *testing.T) {
	cfg := testConfig(t)
	for name, mutate := range map[string]func(*Config){
		"cluster":                    func(c *Config) { c.Cluster = "" },
		"namespace":                  func(c *Config) { c.Namespace = "../namespace" },
		"missing marker name":        func(c *Config) { c.InstallationConfigMapName = "" },
		"aliased durable objects":    func(c *Config) { c.InstallationConfigMapName = c.VersionConfigMapName },
		"aliased credential secrets": func(c *Config) { c.CredentialsSecretName = "" },
		"no preparation":             func(c *Config) { c.Rotation.PrepareFor = 0 },
		"short overlap":              func(c *Config) { c.Rotation.RetainFor = wire.CertificateLifetime - 1 },
		"short interval":             func(c *Config) { c.Rotation.Interval = c.Rotation.PrepareFor - 1 },
		"zero port":                  func(c *Config) { c.PeerPort = 0 },
		"unbounded polls":            func(c *Config) { c.Limits.MaxPolls = 0 },
		"unbounded writes":           func(c *Config) { c.Limits.MaxConcurrentWrites = 0 },
		"unbounded bootstrap":        func(c *Config) { c.Limits.MaxConcurrentBootstrap = 0 },
		"unbounded headers":          func(c *Config) { c.Limits.HeaderBytes = 0 },
		"unbounded write duration":   func(c *Config) { c.Limits.WriteTimeout = 0 },
		"unbounded shutdown":         func(c *Config) { c.Limits.ShutdownTimeout = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)

			if err := invalid.Validate(); !errors.Is(err, wire.InvalidRequest) {
				t.Fatalf("invalid config accepted: %v", err)
			}
		})
	}

	for _, port := range []string{"0", "65536", "-1", "invalid"} {
		t.Setenv("RACER_PEER_PORT", port)

		if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("port %q: %v", port, err)
		}
	}

	t.Setenv("RACER_PEER_PORT", "65535")
	t.Setenv("RACER_INSTALLATION_CONFIGMAP_NAME", "permanent-installation")
	t.Setenv("RACER_CREDENTIALS_SECRET_NAME", "custom-credentials")

	loaded, err := LoadConfig()
	if err != nil || loaded.PeerPort != 65535 || loaded.InstallationConfigMapName != "permanent-installation" || loaded.CredentialsSecretName != "custom-credentials" {
		t.Fatalf("deployment overrides: %+v, %v", loaded, err)
	}
}

func TestRuntimeConfigDoesNotReadWorkloadOnlySettings(t *testing.T) {
	_, err := ConfigFromLookup(func(key string) (string, bool) {
		switch key {
		case "RACER_CLUSTER_ID":
			return "11111111-1111-1111-1111-111111111111", true
		case "RACER_CONTROL_URL", "RACER_DATAPLANE_IMAGE", "RACER_BOOTSTRAP_TRUST_CONFIGMAP":
			t.Errorf("runtime requested workload-only setting %s", key)
			return "invalid", true
		default:
			return "", false
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestConfigShortRotationDurations(t *testing.T) {
	testConfig(t)

	cfg, err := LoadConfig()
	if err != nil || cfg.CertificateLifetime != wire.CertificateLifetime {
		t.Fatalf("default lifetime: %v", err)
	}

	for name, value := range map[string]string{
		"RACER_CERTIFICATE_LIFETIME": "2m",
		"RACER_ROTATION_INTERVAL":    "5m",
		"RACER_ROTATION_PREPARE_FOR": "20s",
		"RACER_ROTATION_RETAIN_FOR":  "2m",
	} {
		t.Setenv(name, value)
	}

	cfg, err = LoadConfig()
	if err != nil || cfg.CertificateLifetime != 2*time.Minute || cfg.Rotation != (RotationPolicy{5 * time.Minute, 20 * time.Second, 2 * time.Minute}) {
		t.Fatalf("short rotation config: %v", err)
	}

	for name, value := range map[string]string{
		"RACER_CERTIFICATE_LIFETIME": "119s",
		"RACER_ROTATION_INTERVAL":    "19s",
		"RACER_ROTATION_PREPARE_FOR": "500ms",
		"RACER_ROTATION_RETAIN_FOR":  "119s",
	} {
		t.Run(name, func(t *testing.T) {
			for _, invalid := range []string{value, "", "nonsense", "0", "-1s", "8761h", "120.5s"} {
				t.Setenv(name, invalid)

				if _, err := LoadConfig(); !errors.Is(err, wire.InvalidRequest) {
					t.Fatalf("%s=%q accepted: %v", name, invalid, err)
				}
			}
		})
	}
}

func TestConfigDurationsUseProvidedLookup(t *testing.T) {
	values := map[string]string{
		"RACER_CLUSTER_ID":           string(testConfig(t).Cluster),
		"RACER_CERTIFICATE_LIFETIME": "2m",
		"RACER_ROTATION_INTERVAL":    "5m",
		"RACER_ROTATION_PREPARE_FOR": "1m",
		"RACER_ROTATION_RETAIN_FOR":  "2m",
	}
	for name := range values {
		t.Setenv(name, "invalid-process-value")
	}

	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := ConfigFromLookup(lookup)
	if err != nil || cfg.CertificateLifetime != 2*time.Minute || cfg.Rotation != (RotationPolicy{5 * time.Minute, time.Minute, 2 * time.Minute}) {
		t.Fatalf("custom lookup ignored: %v", err)
	}

	for _, name := range []string{"RACER_CERTIFICATE_LIFETIME", "RACER_ROTATION_INTERVAL", "RACER_ROTATION_PREPARE_FOR", "RACER_ROTATION_RETAIN_FOR"} {
		previous := values[name]

		values[name] = "invalid-lookup-value"
		if _, err := ConfigFromLookup(lookup); !errors.Is(err, wire.InvalidRequest) {
			t.Fatalf("invalid custom %s accepted: %v", name, err)
		}

		values[name] = previous
	}

	delete(values, "RACER_CERTIFICATE_LIFETIME")
	delete(values, "RACER_ROTATION_RETAIN_FOR")

	cfg, err = ConfigFromLookup(lookup)
	if err != nil || cfg.CertificateLifetime != wire.CertificateLifetime || cfg.Rotation.RetainFor != 48*time.Hour {
		t.Fatalf("absent custom values did not use defaults: %v", err)
	}
}

func TestReplicationConfigDefaultsAndOverrides(t *testing.T) {
	values := map[string]string{"RACER_CLUSTER_ID": string(testConfig(t).Cluster), "POD_NAMESPACE": "controllers"}
	lookup := func(key string) (string, bool) { value, ok := values[key]; return value, ok }

	cfg, err := ConfigFromLookup(lookup)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.SnapshotMaxAge != 30*time.Second || cfg.ReplicationPort != 8443 || cfg.ReplicationServerName != "racer-controller.controllers.svc" || cfg.ReplicationTokenFile != "/var/run/secrets/racer-controller/token" || cfg.ReplicationTrustFile != "/etc/racer/tls/ca.crt" || cfg.ControllerServiceAccount != "racer-controller" {
		t.Fatalf("replication defaults: %+v", cfg)
	}

	values["RACER_SNAPSHOT_MAX_AGE"] = "45s"
	values["RACER_REPLICATION_PORT"] = "9443"
	values["POD_NAME"] = "controller-0"
	values["POD_UID"] = "pod-uid"

	cfg, err = ConfigFromLookup(lookup)
	if err != nil || cfg.SnapshotMaxAge != 45*time.Second || cfg.ReplicationPort != 9443 || cfg.PodName != "controller-0" || cfg.PodUID != "pod-uid" {
		t.Fatal("replication overrides", err)
	}

	for name, invalid := range map[string][]string{"RACER_REPLICATION_PORT": {"0", "65536", "-1", "bad"}, "RACER_SNAPSHOT_MAX_AGE": {"0s", "-1s", "500ms", "bad"}} {
		previous := values[name]
		for _, value := range invalid {
			values[name] = value
			if _, err := ConfigFromLookup(lookup); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		}

		values[name] = previous
	}
}
