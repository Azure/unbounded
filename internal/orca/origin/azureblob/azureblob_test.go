// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package azureblob

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/Azure/unbounded/internal/orca/config"
	"github.com/Azure/unbounded/internal/orca/origin"
)

// TestValidateBlobType covers every branch of the unconditional
// block-blob-only enforcement. PageBlob and AppendBlob are always
// rejected; BlockBlob and the nil/unknown response shape pass.
func TestValidateBlobType(t *testing.T) {
	pageBlob := blob.BlobTypePageBlob
	appendBlob := blob.BlobTypeAppendBlob
	blockBlob := blob.BlobTypeBlockBlob

	tests := []struct {
		name            string
		blobType        *blob.BlobType
		wantUnsupported bool
	}{
		{"nil blob type passes (no info to validate)", nil, false},
		{"block blob accepted", &blockBlob, false},
		{"page blob refused", &pageBlob, true},
		{"append blob refused", &appendBlob, true},
	}

	const (
		container = "ctr"
		key       = "key"
	)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateBlobType(container, key, tt.blobType)

			if (err != nil) != tt.wantUnsupported {
				t.Fatalf("err=%v, wantUnsupported=%v", err, tt.wantUnsupported)
			}

			if !tt.wantUnsupported {
				return
			}

			var ube *origin.UnsupportedBlobTypeError
			if !errors.As(err, &ube) {
				t.Fatalf("err type=%T (want *origin.UnsupportedBlobTypeError): %v", err, err)
			}

			if ube.Bucket != container {
				t.Errorf("Bucket=%q want %q", ube.Bucket, container)
			}

			if ube.Key != key {
				t.Errorf("Key=%q want %q", ube.Key, key)
			}

			if tt.blobType != nil && ube.BlobType != string(*tt.blobType) {
				t.Errorf("BlobType=%q want %q", ube.BlobType, string(*tt.blobType))
			}
		})
	}
}

// TestValidateBlobType_NonBlockBlob_AlwaysRejected is the regression
// test for the fix that removed the user-overridable
// EnforceBlockBlobOnly flag. There is no longer any code path that
// accepts a Page or Append blob.
func TestValidateBlobType_NonBlockBlob_AlwaysRejected(t *testing.T) {
	pageBlob := blob.BlobTypePageBlob

	if err := validateBlobType("ctr", "key", &pageBlob); err == nil {
		t.Fatalf("page blob accepted; want UnsupportedBlobTypeError")
	}

	appendBlob := blob.BlobTypeAppendBlob
	if err := validateBlobType("ctr", "key", &appendBlob); err == nil {
		t.Fatalf("append blob accepted; want UnsupportedBlobTypeError")
	}
}

// TestGetRange_QuotesIfMatchHeader verifies that the If-Match header
// emitted on a conditional GetRange is the etag value wrapped in
// double quotes per RFC 7232. The internal representation strips
// quotes on Head (drivers normalize to unquoted), so this is the
// re-wrap point on egress. Without the wrap an upstream that
// strictly enforces RFC 7232 entity-tag syntax would reject the
// precondition or treat it as never-matched.
func TestGetRange_QuotesIfMatchHeader(t *testing.T) {
	t.Parallel()

	const etagUnquoted = "0x8DDCAFE00000000"

	var captured atomic.Value // string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Store(r.Header.Get("If-Match"))
		// Respond with the requested bytes. The exact body is not
		// validated by this test - only the inbound If-Match header
		// is. A small synthetic body keeps the SDK happy.
		w.Header().Set("Content-Length", "4")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("ETag", "\""+etagUnquoted+"\"")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("test")) //nolint:errcheck // best-effort test write
	}))

	t.Cleanup(srv.Close)
	// Azurite uses the account name as the URL path component. We
	// mirror that shape so the SDK signs/issues requests in the
	// expected layout.
	cfg := config.Azureblob{
		Account:    "devstoreaccount1",
		AccountKey: base64.StdEncoding.EncodeToString([]byte("test-shared-key-placeholder--32b")),
		Container:  "ctr",
		Endpoint:   srv.URL + "/devstoreaccount1",
	}

	a, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("azureblob.New: %v", err)
	}

	body, err := a.GetRange(context.Background(), "ctr", "key", etagUnquoted, 0, 4)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}

	defer body.Close() //nolint:errcheck // test cleanup

	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("read body: %v", err)
	}

	got, _ := captured.Load().(string)

	want := "\"" + etagUnquoted + "\""
	if got != want {
		t.Errorf("If-Match=%q want %q", got, want)
	}
}

// TestGetRange_OmitsIfMatchWhenEtagEmpty verifies that the If-Match
// header is not sent at all when the caller supplies an empty etag.
// Sending an empty If-Match would either be a malformed precondition
// or evaluate as never-matching depending on server interpretation.
func TestGetRange_OmitsIfMatchWhenEtagEmpty(t *testing.T) {
	t.Parallel()

	var captured atomic.Value // string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record presence/absence; empty string here means "header
		// was absent".
		captured.Store(r.Header.Get("If-Match"))
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("test")) //nolint:errcheck // best-effort test write
	}))

	t.Cleanup(srv.Close)

	cfg := config.Azureblob{
		Account:    "devstoreaccount1",
		AccountKey: base64.StdEncoding.EncodeToString([]byte("test-shared-key-placeholder--32b")),
		Container:  "ctr",
		Endpoint:   srv.URL + "/devstoreaccount1",
	}

	a, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("azureblob.New: %v", err)
	}

	body, err := a.GetRange(context.Background(), "ctr", "key", "", 0, 4)
	if err != nil {
		t.Fatalf("GetRange: %v", err)
	}

	defer body.Close() //nolint:errcheck // test cleanup

	_, _ = io.ReadAll(body) //nolint:errcheck // test cleanup

	got, _ := captured.Load().(string)
	if got != "" {
		t.Errorf("If-Match present (%q) when etag was empty; want absent", got)
	}
}
