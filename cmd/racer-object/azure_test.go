// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestProviderOptions(t *testing.T) {
	for _, provider := range []string{"s3", "azure"} {
		args := []string{"origin", testCacheArg, "--bucket=bucket", "--provider=" + provider}
		if provider == "azure" {
			args = []string{"origin", testCacheArg, "--provider=azure", "--container-url=https://account.blob.core.windows.net/bucket"}
		}

		o, err := parseOptions(args, io.Discard)
		if err != nil || o.provider != provider || o.azureCredential != "default" || o.bucket != "bucket" {
			t.Fatalf("provider %s: %+v, %v", provider, o, err)
		}
	}

	o, err := parseOptions([]string{"origin", testCacheArg, "--bucket=bucket"}, io.Discard)
	if err != nil || o.provider != "s3" {
		t.Fatalf("S3 default: %+v, %v", o, err)
	}

	o, err = parseOptions([]string{"origin", testCacheArg, "--provider=azure", "--container-url=http://localhost:10000/devstoreaccount1/container", "--azure-credential=anonymous", "--metadata-ttl=2s", "--request-timeout=3s"}, io.Discard)
	if err != nil || o.azureCredential != "anonymous" || o.containerURL != "http://localhost:10000/devstoreaccount1/container" ||
		o.bucket != "container" || o.metadataTTL != 2*time.Second || o.requestTimeout != 3*time.Second {
		t.Fatalf("Azure options: %+v, %v", o, err)
	}

	for _, api := range []string{"", "s3", "azure"} {
		args := []string{"sidecar", testCacheArg}

		want := api
		if api == "" {
			want = "s3"
		} else {
			args = append(args, "--api="+api)
		}

		o, err := parseOptions(args, io.Discard)
		if err != nil || o.api != want {
			t.Fatalf("API %q: %+v, %v", api, o, err)
		}
	}
}

func TestProviderFlagConflicts(t *testing.T) {
	for _, extra := range [][]string{
		{"--provider=azure", "--container-url=https://account/bucket", "--region="},
		{"--provider=azure", "--container-url=https://account/bucket", "--endpoint="},
		{"--provider=azure", "--container-url=https://account/bucket", "--path-style=true"},
		{"--provider=azure", "--container-url=https://account/bucket", "--path-style=false"},
		{"--provider=azure", "--container-url=https://account/bucket", "--bucket=bucket"},
		{"--provider=azure", "--container-url=https://account/bucket", "--bucket="},
		{"--bucket=bucket", "--container-url="},
		{"--bucket=bucket", "--container-url=https://account/bucket"},
		{"--bucket=bucket", "--azure-credential=default"},
		{"--bucket=bucket", "--provider=s3", "--azure-credential=anonymous"},
		{"--provider=azure"},
		{"--provider=azure", "--container-url="},
		{"--provider=azure", "--container-url=https://account"},
		{"--provider=azure", "--container-url=https://account/"},
		{"--provider=azure", "--container-url=https://account/Bucket"},
		{"--provider=azure", "--container-url=https://account/bucket/"},
		{"--provider=azure", "--container-url=https://account/a/b/c"},
		{"--provider=azure", "--container-url=http://account/bucket"},
		{"--provider=azure", "--container-url=https://account/bucket", "--azure-credential="},
		{"--provider=azure", "--container-url=https://account/bucket", "--azure-credential=shared-key"},
		{"--provider=azure", "--container-url=https://account/bucket", "--azure-credential=sas"},
		{"--bucket=bucket", "--provider="},
		{"--bucket=bucket", "--provider=Azure"},
		{"--bucket=bucket", "--provider=other"},
		{"--bucket=bucket", "--api=azure"},
		{},
	} {
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			args := append([]string{"origin", testCacheArg}, extra...)
			if _, err := parseOptions(args, io.Discard); err == nil {
				t.Fatal("accepted invalid flags")
			}
		})
	}

	for _, extra := range []string{"--api=", "--api=Azure", "--api=other", "--provider=azure", "--container-url=", "--azure-credential=default"} {
		if _, err := parseOptions([]string{"sidecar", testCacheArg, extra}, io.Discard); err == nil {
			t.Errorf("accepted %s", extra)
		}
	}
}

func TestAzureCacheOptions(t *testing.T) {
	for _, args := range [][]string{
		{"origin", testCacheArg, "--provider=azure", "--container-url=https://account.blob.core.windows.net/bucket"},
		{"sidecar", testCacheArg, "--api=azure"},
	} {
		t.Run(args[0], func(t *testing.T) {
			o, err := parseOptions(append(args, "--cache=azure-test"), io.Discard)
			if err != nil || o.cache != "azure-test" {
				t.Fatalf("Azure cache: %+v, %v", o, err)
			}
		})
	}
}

func TestAzureHelpAndRedaction(t *testing.T) {
	for _, mode := range []string{"origin", "sidecar"} {
		var output bytes.Buffer

		_, err := parseOptions([]string{mode, "--help"}, &output)
		if !errors.Is(err, flag.ErrHelp) || strings.Contains(output.String(), "-container-url") != (mode == "origin") ||
			strings.Contains(output.String(), "-namespace") {
			t.Fatalf("help: %q, %v", output.String(), err)
		}

		flags := []string{"-api"}
		if mode == "origin" {
			flags = []string{"-provider", "-bucket", "-container-url", "-azure-credential"}
		}

		for _, name := range flags {
			if !strings.Contains(output.String(), name) {
				t.Errorf("missing %s in help", name)
			}
		}
	}

	for _, extra := range []string{"--container-url=https://account/bucket?sig=private-token", "--container-url=https://private-token@account/bucket", "--azure-credential=private-token", "--provider=private-token"} {
		var output bytes.Buffer

		err := run(t.Context(), []string{"origin", testCacheArg, "--provider=azure", extra}, &output)
		if err == nil || strings.Contains(err.Error()+output.String(), "private-token") {
			t.Fatalf("invalid argument accepted or leaked: %v", err)
		}
	}
}

func TestCreateAzureOriginAnonymous(t *testing.T) {
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "missing"))
	t.Setenv("AWS_PROFILE", "missing-profile")
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "invalid-credential")

	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if r.Method != http.MethodHead || r.URL.Path != "/devstoreaccount1/container/key" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}

		if r.Header.Get("Authorization") != "" || !strings.Contains(r.UserAgent(), "racer-object") {
			t.Errorf("unexpected auth or telemetry: %v", r.Header)
		}

		w.Header().Set("ETag", `"version"`)
		w.Header().Set("Content-Length", "5")
		w.Header().Set("Content-Type", "application/test")
		w.Header().Set("x-ms-blob-type", "BlockBlob")
	}))
	defer server.Close()

	o, err := parseOptions([]string{"origin", testCacheArg, "--provider=azure", "--azure-credential=anonymous", "--container-url=" + server.URL + "/devstoreaccount1/container", "--metadata-ttl=2s"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	origin, err := createOrigin(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}

	request, err := racerobject.NewRequest("container", "key", "")
	if err != nil {
		t.Fatal(err)
	}

	before := time.Now()

	metadata, body, err := origin(t.Context(), racersdk.OriginRequest{Request: request, Head: true})
	if body != nil {
		body.Close()
	}

	if err != nil || body != nil || metadata.Size != 5 || metadata.ETag != `"version"` || metadata.ExpiresAt.Before(before.Add(time.Second)) || calls.Load() != 1 {
		t.Fatalf("Azure origin: %+v, body=%v, err=%v, calls=%d", metadata, body, err, calls.Load())
	}

	request, err = racerobject.NewRequest("other", "key", "")
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = origin(t.Context(), racersdk.OriginRequest{Request: request, Head: true})
	if err == nil || calls.Load() != 1 {
		t.Fatal("container restriction not applied")
	}
}

func TestAzureDefaultCredentialConstruction(t *testing.T) {
	for _, name := range []string{"AZURE_CLIENT_SECRET", "AZURE_CLIENT_CERTIFICATE_PATH", "AZURE_FEDERATED_TOKEN_FILE", "AZURE_TENANT_ID", "AZURE_TOKEN_CREDENTIALS"} {
		t.Setenv(name, "")

		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("AZURE_CLIENT_ID", "00000000-0000-0000-0000-000000000001")

	o := options{provider: "azure", bucket: "bucket", containerURL: "https://account.blob.core.windows.net/bucket", azureCredential: "default", requestTimeout: 2 * time.Second, maxRequests: 7}

	clientOptions := originAzureClientOptions(o)
	if clientOptions.Transport.(*http.Client).Timeout != 2*time.Second || clientOptions.Telemetry.ApplicationID != "racer-object" {
		t.Fatal("Azure transport timeout or telemetry missing")
	}

	transport, ok := clientOptions.Transport.(*http.Client).Transport.(*http.Transport)
	if !ok || transport.MaxIdleConnsPerHost != 7 || transport.MaxIdleConns != 7 || transport == http.DefaultTransport {
		t.Fatal("Azure transport idle pool not sized to the origin request limit")
	}
	// Construction must not request a token or contact Azure.
	if origin, err := createOrigin(t.Context(), o); err != nil || origin == nil {
		t.Fatalf("default credential construction: %v", err)
	}

	t.Setenv("AZURE_TOKEN_CREDENTIALS", "private-token")

	if _, err := createOrigin(t.Context(), o); err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatalf("credential construction failure lost or leaked: %v", err)
	}
}

func TestCreateOriginS3Selection(t *testing.T) {
	dir := t.TempDir()
	for key, value := range map[string]string{
		"AWS_CONFIG_FILE": filepath.Join(dir, "config"), "AWS_SHARED_CREDENTIALS_FILE": filepath.Join(dir, "credentials"),
		"AWS_PROFILE": "", "AWS_DEFAULT_PROFILE": "", "AWS_REGION": "", "AWS_DEFAULT_REGION": "", "AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(key, value)
	}

	o, err := parseOptions([]string{"origin", testCacheArg, "--bucket=bucket"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := createOrigin(t.Context(), o); err == nil || !strings.Contains(err.Error(), "AWS region") {
		t.Fatalf("default provider did not select S3: %v", err)
	}

	o.region = "us-east-1"
	if origin, err := createOrigin(t.Context(), o); err != nil || origin == nil {
		t.Fatalf("S3 construction: %v", err)
	}
}

func TestCreateSidecarAPISelection(t *testing.T) {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, api := range []string{"s3", "azure"} {
		o, err := parseOptions([]string{"sidecar", testCacheArg, "--api=" + api}, io.Discard)
		if err != nil {
			t.Fatal(err)
		}

		handler, err := createSidecar(client, o)
		if err != nil {
			t.Fatal(err)
		}

		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/container/key", nil))

		if response.Code != http.StatusMethodNotAllowed || (response.Header().Get("x-ms-version") != "") != (api == "azure") {
			t.Fatalf("API %s: status=%d, headers=%v", api, response.Code, response.Header())
		}
	}
}
