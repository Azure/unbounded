// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

func azureSidecarTestHandler(t *testing.T, origin racersdk.Origin) http.Handler {
	t.Helper()
	client := racersdktest.NewClient(t, origin)

	h, err := NewSidecar(client, SidecarConfig{API: "azure", Namespace: "test", Buckets: []string{"bucket"}})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func TestAzureSidecarReads(t *testing.T) {
	var calls atomic.Int32

	h := azureSidecarTestHandler(t, sidecarTestOrigin(t, "0123456789", `"v"`, &calls))
	for _, tc := range []struct {
		name, method, standard, azure, body, contentRange string
		status, length                                    int
	}{
		{"full", "GET", "", "", "0123456789", "", 200, 10},
		{"head", "HEAD", "", "", "", "", 200, 10},
		{"standard", "GET", "bytes=2-4", "", "234", "bytes 2-4/10", 206, 3},
		{"azure", "GET", "", "bytes=2-4", "234", "bytes 2-4/10", 206, 3},
		{"precedence", "GET", "bad", "bytes=7-", "789", "bytes 7-9/10", 206, 3},
		{"suffix", "GET", "", "bytes=-2", "89", "bytes 8-9/10", 206, 2},
		{"clamp", "GET", "", "bytes=8-99", "89", "bytes 8-9/10", 206, 2},
		{"head ignores ranges", "HEAD", "bad", "bytes=999-", "", "", 200, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := http.Header{"Authorization": {"Bearer ignored-secret"}}
			if tc.standard != "" {
				headers.Set("Range", tc.standard)
			}

			if tc.azure != "" {
				headers.Set("x-ms-range", tc.azure)
			}

			before := calls.Load()

			w := sidecarTestRequest(h, tc.method, "/bucket/blob?versionid=v%2B%2F%25&timeout=30", headers)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("response = %d %q", w.Code, w.Body.String())
			}

			for name, want := range map[string]string{
				"ETag": `"v"`, "Content-Length": strconv.Itoa(tc.length), "Content-Type": "text/plain",
				"Accept-Ranges": "bytes", "Content-Range": tc.contentRange, "x-ms-blob-type": "BlockBlob",
				"x-ms-version": azureSidecarVersion, "x-ms-version-id": "v+/%", "Last-Modified": "", "x-amz-version-id": "",
			} {
				if got := w.Header().Get(name); got != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}

			if tc.method == "HEAD" && calls.Load()-before != 1 {
				t.Fatal("HEAD opened a stream")
			}
		})
	}
}

func TestAzureSidecarConditionsAndRanges(t *testing.T) {
	h := azureSidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, nil))

	for _, tc := range []struct {
		header, value string
		status        int
		code          string
	}{
		{"If-Match", `"v"`, 200, ""},
		{"If-Match", `"other", "v"`, 200, ""},
		{"If-Match", "*", 200, ""},
		{"If-Match", `W/"v"`, 412, "ConditionNotMet"},
		{"If-Match", `"other"`, 412, "ConditionNotMet"},
		{"If-Match", "bare", 400, "InvalidInput"},
		{"If-None-Match", `"v"`, 304, ""},
		{"If-None-Match", `W/"v"`, 304, ""},
		{"If-None-Match", "*", 304, ""},
		{"If-None-Match", `"other"`, 200, ""},
		{"x-ms-range", "bytes=4-", 416, "InvalidRange"},
		{"x-ms-range", "bytes=-0", 416, "InvalidRange"},
		{"x-ms-range", "bytes=2-1", 400, "InvalidInput"},
		{"x-ms-range", "bytes=0-1,2-3", 400, "InvalidInput"},
		{"x-ms-range", "", 400, "InvalidInput"},
		{"x-ms-range", "bytes=18446744073709551616-", 400, "InvalidInput"},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(method+tc.header+tc.value, func(t *testing.T) {
				headers := http.Header{}
				headers.Set(tc.header, tc.value)
				w := sidecarTestRequest(h, method, "/bucket/blob", headers)

				status, code := tc.status, tc.code
				if method == "HEAD" && tc.header == "x-ms-range" {
					status, code = 200, ""
				}

				if w.Code != status || w.Header().Get("x-ms-error-code") != code {
					t.Fatalf("response: %d %v", w.Code, w.Header())
				}

				if method == "HEAD" || status == 304 {
					if w.Body.Len() != 0 {
						t.Fatal("unexpected body")
					}
				}

				if status == 304 && w.Header().Get("ETag") != `"v"` {
					t.Fatal("missing 304 ETag")
				}

				if status == 416 && w.Header().Get("Content-Range") != "bytes */4" {
					t.Fatal("missing range size")
				}
			})
		}
	}

	for _, headers := range []http.Header{
		{"X-Ms-Range": {"bytes=0-1", "bytes=2-3"}},
		{"X-Ms-Range": {""}, "Range": {"bytes=0-1"}},
	} {
		if w := sidecarTestRequest(h, "GET", "/bucket/blob", headers); w.Code != 400 {
			t.Fatalf("invalid Azure range accepted: %d", w.Code)
		}
	}
}

func TestAzureSidecarRejectedRequests(t *testing.T) {
	var calls atomic.Int32

	h := azureSidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, &calls))

	for _, tc := range []struct {
		target string
		status int
	}{
		{"/", 400},
		{"/bucket", 400},
		{"/bucket/", 400},
		{"/bucket%2Fblob", 400},
		{"/other/blob", 403},
		{"/account/bucket/blob", 403},
		{"/bucket/%FF", 400},
		{"/bucket/blob?versionid=", 400},
		{"/bucket/blob?versionid=a&versionid=b", 400},
		{"/bucket/blob?versionid=%zz", 400},
		{"/bucket/blob?versionid=a;b", 400},
		{"/bucket/blob?versionid=%00", 400},
		{"/bucket/blob?versionid=%7F", 400},
		{"/bucket/blob?versionid=%FF", 400},
		{"/bucket/blob?timeout=0", 400},
		{"/bucket/blob?timeout=-1", 400},
		{"/bucket/blob?timeout=1.5", 400},
		{"/bucket/blob?timeout=%2B1", 400},
		{"/bucket/blob?timeout=", 400},
		{"/bucket/blob?timeout=1&timeout=2", 400},
		{"/bucket/blob?timeout=18446744073709551616", 400},
		{"/bucket/blob?snapshot=x", 501},
		{"/bucket/blob?sig=secret", 501},
		{"/bucket/blob?sv=x", 501},
		{"/bucket/blob?comp=metadata", 501},
		{"/bucket/blob?restype=container", 501},
		{"/bucket/blob?versionId=x", 501},
		{"/bucket/blob?unknown=x", 501},
		{"/bucket/blob?unknown=", 400},
		{"/bucket/blob?unknown=a&unknown=b", 400},
		{"/bucket/blob?comp=metadata&timeout=bad", 400},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(method+tc.target, func(t *testing.T) {
				w := sidecarTestRequest(h, method, tc.target, nil)
				assertAzureSidecarError(t, w, method, tc.status, azureSidecarCodes[sidecarFailure(tc.status)])
			})
		}
	}

	for _, name := range []string{"If-Modified-Since", "If-Unmodified-Since", "If-Range", "x-ms-lease-id", "x-ms-encryption-key", "x-ms-encryption-key-sha256", "x-ms-encryption-algorithm", "x-ms-encryption-scope", "x-ms-if-tags", "x-ms-range-get-content-md5", "x-ms-range-get-content-crc64", "x-ms-structured-body", "x-ms-unknown"} {
		for _, method := range []string{"GET", "HEAD"} {
			headers := http.Header{}
			headers.Set(name, "")
			w := sidecarTestRequest(h, method, "/bucket/blob", headers)
			assertAzureSidecarError(t, w, method, 501, "NotImplemented")
		}
	}

	for _, method := range []string{"PUT", "POST", "DELETE", "OPTIONS"} {
		w := sidecarTestRequest(h, method, "/bucket/blob", nil)
		assertAzureSidecarError(t, w, method, 405, "UnsupportedHttpVerb")

		if w.Header().Get("Allow") != "GET, HEAD" {
			t.Fatal("missing Allow")
		}
	}

	if calls.Load() != 0 {
		t.Fatal("rejected request reached Racer")
	}
}

func assertAzureSidecarError(t *testing.T, w *httptest.ResponseRecorder, method string, status int, code string) {
	t.Helper()

	if w.Code != status || w.Header().Get("x-ms-error-code") != code || w.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("error response = %d %v %s", w.Code, w.Header(), w.Body.String())
	}

	if method == "HEAD" {
		if w.Body.Len() != 0 {
			t.Fatal("HEAD error body")
		}

		return
	}

	var parsed struct {
		XMLName       xml.Name
		Code, Message string
	}
	if err := xml.Unmarshal(w.Body.Bytes(), &parsed); err != nil || parsed.XMLName.Local != "Error" || parsed.Code != code || parsed.Message != http.StatusText(status) {
		t.Fatalf("error XML: %s (%v)", w.Body.String(), err)
	}
}

func TestAzureSidecarExactIdentity(t *testing.T) {
	for _, key := range []string{"a//b/../c", "/leading", "./relative", "a%2Fb", "a+b c", "雪/☃", "trailing/", "a?b#c", "a\x7fb"} {
		t.Run(key, func(t *testing.T) {
			want, err := NewRequest("test", "bucket", key, "version+/%")
			if err != nil {
				t.Fatal(err)
			}

			origin := sidecarTestOrigin(t, "body", `"v"`, nil)
			h := azureSidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if r.Key != want.Key || r.Metadata != want.Metadata {
					t.Error("exact identity changed")
				}

				return origin(ctx, r)
			})

			w := sidecarTestRequest(h, "GET", "/bucket/"+url.PathEscape(key)+"?versionid="+url.QueryEscape("version+/%"), nil)
			if w.Code != 200 || w.Body.String() != "body" {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestAzureSidecarSDKFailures(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{racersdk.ErrInvalidRequest, 400, "InvalidInput"},
		{racersdk.ErrUnauthorized, 403, "AuthorizationFailure"},
		{racersdk.ErrForbidden, 403, "AuthorizationFailure"},
		{racersdk.ErrNotFound, 404, "BlobNotFound"},
		{racersdk.ErrVersionMismatch, 412, "ConditionNotMet"},
		{errors.New("private upstream"), 502, "BadGateway"},
		{racersdk.ErrUnavailable, 503, "ServerBusy"},
		{context.Canceled, 503, "ServerBusy"},
		{context.DeadlineExceeded, 503, "ServerBusy"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			h := azureSidecarTestHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				return racersdk.Metadata{}, nil, fmt.Errorf("private secret: %w", tc.err)
			})
			for _, method := range []string{"GET", "HEAD"} {
				assertAzureSidecarError(t, sidecarTestRequest(h, method, "/bucket/blob", nil), method, tc.status, tc.code)
			}
		})
	}
}

func TestAzureSidecarRealSDK(t *testing.T) {
	const key, version = "a//b/../c%2F +雪", "version+/%"

	want, err := NewRequest("test", "bucket", key, version)
	if err != nil {
		t.Fatal(err)
	}

	origin := sidecarTestOrigin(t, "0123456789", `"v"`, nil)
	h := azureSidecarTestHandler(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Key != want.Key || r.Metadata != want.Metadata {
			t.Error("SDK changed exact path/version/scope")
		}

		return origin(ctx, r)
	})

	var signed, userAgent atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.Header.Get("Authorization"), "SharedKey ") {
			signed.Store(true)
		}

		if strings.Contains(r.Header.Get("User-Agent"), "azsdk-go-azblob/") {
			userAgent.Store(true)
		}

		if r.Header.Get("x-ms-version") == "" || r.Header.Get("x-ms-date") == "" {
			t.Error("missing SDK plumbing headers")
		}

		h.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)

	credential, err := blob.NewSharedKeyCredential("account", "a2V5")
	if err != nil {
		t.Fatal(err)
	}

	client, err := blob.NewClientWithSharedKeyCredential(server.URL+"/bucket/"+url.PathEscape(key)+"?versionid="+url.QueryEscape(version), credential,
		&blob.ClientOptions{ClientOptions: azcore.ClientOptions{Transport: server.Client(), Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}

	properties, err := client.GetProperties(t.Context(), nil)
	if err != nil || properties.ContentLength == nil || *properties.ContentLength != 10 || properties.BlobType == nil || *properties.BlobType != blob.BlobTypeBlockBlob || properties.LastModified != nil {
		t.Fatalf("properties = %+v, %v", properties, err)
	}

	response, err := client.DownloadStream(t.Context(), &blob.DownloadStreamOptions{
		Range: blob.HTTPRange{Offset: 2, Count: 3}, AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(azcore.ETag(`"v"`))}},
	})
	if err != nil {
		t.Fatal(err)
	}

	data, readErr := io.ReadAll(response.Body)

	_ = response.Body.Close()
	if readErr != nil || string(data) != "234" || response.ContentRange == nil || *response.ContentRange != "bytes 2-4/10" || response.VersionID == nil || *response.VersionID != version {
		t.Fatalf("download = %q, %+v, %v", data, response, readErr)
	}

	buffer := make([]byte, 10)

	n, err := client.DownloadBuffer(t.Context(), buffer, &blob.DownloadBufferOptions{BlockSize: 3, Concurrency: 2})
	if err != nil || n != 10 || string(buffer) != "0123456789" {
		t.Fatalf("buffer = %q, %d, %v", buffer, n, err)
	}

	for _, head := range []bool{false, true} {
		conditions := &blob.AccessConditions{ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfMatch: to.Ptr(azcore.ETag(`"other"`))}}
		if head {
			_, err = client.GetProperties(t.Context(), &blob.GetPropertiesOptions{AccessConditions: conditions})
		} else {
			_, err = client.DownloadStream(t.Context(), &blob.DownloadStreamOptions{AccessConditions: conditions})
		}

		var responseError *azcore.ResponseError
		if !errors.As(err, &responseError) || responseError.ErrorCode != "ConditionNotMet" || responseError.StatusCode != 412 {
			t.Fatalf("SDK condition error: %v", err)
		}
	}

	_, err = client.DownloadStream(t.Context(), &blob.DownloadStreamOptions{RangeGetContentMD5: to.Ptr(true), Range: blob.HTTPRange{Count: 3}})

	var responseError *azcore.ResponseError
	if !errors.As(err, &responseError) || responseError.ErrorCode != "NotImplemented" {
		t.Fatalf("SDK checksum error: %v", err)
	}

	_, err = client.DownloadStream(t.Context(), &blob.DownloadStreamOptions{Range: blob.HTTPRange{Offset: 10}})
	if !errors.As(err, &responseError) || responseError.ErrorCode != "InvalidRange" || responseError.StatusCode != 416 {
		t.Fatalf("SDK range error: %v", err)
	}

	notModified, err := client.DownloadStream(t.Context(), &blob.DownloadStreamOptions{AccessConditions: &blob.AccessConditions{
		ModifiedAccessConditions: &blob.ModifiedAccessConditions{IfNoneMatch: to.Ptr(azcore.ETag(`"v"`))},
	}})
	if err != nil {
		t.Fatalf("SDK not-modified response: %v", err)
	}

	if notModified.Body != nil {
		data, err := io.ReadAll(notModified.Body)
		_ = notModified.Body.Close()

		if err != nil || len(data) != 0 {
			t.Fatalf("SDK not-modified body: %q, %v", data, err)
		}
	}

	if !signed.Load() || !userAgent.Load() {
		t.Fatal("SDK auth/plumbing was not exercised")
	}
}

func TestAzureSidecarEmptyAndAbort(t *testing.T) {
	h := azureSidecarTestHandler(t, sidecarTestOrigin(t, "", `"v"`, nil))
	for _, method := range []string{"GET", "HEAD"} {
		w := sidecarTestRequest(h, method, "/bucket/blob", nil)
		if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("Content-Length") != "0" {
			t.Fatal("empty response", w)
		}
	}

	m := sidecarTestMetadata(t, 1024*1024, `"v"`)
	h = azureSidecarTestHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Head {
			return m, nil, nil
		}

		return m, io.NopCloser(strings.NewReader(strings.Repeat("x", 512*1024))), nil
	})
	w := httptest.NewRecorder()

	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("panic = %v", got)
		}

		if strings.Contains(w.Body.String(), "<Error>") {
			t.Error("error XML appended to blob")
		}
	}()

	h.ServeHTTP(w, httptest.NewRequest("GET", "/bucket/blob", nil))
	t.Error("failed stream returned normally")
}

func TestSidecarAPIConfig(t *testing.T) {
	client := racersdktest.NewClient(t, sidecarTestOrigin(t, "data", `"v"`, nil))
	for _, api := range []string{"", "s3", "azure", "Azure", "unknown"} {
		h, err := NewSidecar(client, SidecarConfig{Namespace: "test", API: api})
		if api == "Azure" || api == "unknown" {
			if err == nil {
				t.Fatal("accepted API", api)
			}

			continue
		}

		if err != nil {
			t.Fatal(err)
		}

		w := sidecarTestRequest(h, "GET", "/bucket/blob", nil)
		if w.Code != 200 || (w.Header().Get("x-ms-blob-type") != "") != (api == "azure") {
			t.Fatal("wrong API", api, w)
		}
	}
}

func TestAzureSidecarScopeAndPlumbing(t *testing.T) {
	var calls atomic.Int32

	origin := sidecarTestOrigin(t, "data", `"v"`, &calls)

	want, err := NewRequest("account-scope", "bucket", "blob", "")
	if err != nil {
		t.Fatal(err)
	}

	client := racersdktest.NewClient(t, func(ctx context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if r.Request != want {
			t.Error("scope, identity or credentials changed")
		}

		return origin(ctx, r)
	})
	buckets := []string{"bucket"}

	h, err := NewSidecar(client, SidecarConfig{API: "azure", Namespace: "account-scope", Buckets: buckets})
	if err != nil {
		t.Fatal(err)
	}

	buckets[0] = "other"

	headers := http.Header{}
	for _, name := range []string{"Authorization", "x-ms-version", "x-ms-date", "x-ms-client-request-id", "x-ms-useragent", "User-Agent"} {
		headers.Set(name, "ignored")
	}

	w := sidecarTestRequest(h, "GET", "/bucket/blob?timeout=001", headers)
	if w.Code != 200 || w.Header().Get("x-ms-version-id") != "" {
		t.Fatalf("plumbing rejected or version invented: %d %v", w.Code, w.Header())
	}

	before := calls.Load()
	w = sidecarTestRequest(h, "GET", "/other/blob", nil)
	assertAzureSidecarError(t, w, "GET", 403, "AuthorizationFailure")

	if calls.Load() != before {
		t.Fatal("allowlist was not copied")
	}
}

func TestAzureSidecarDotNetRequestCorrelation(t *testing.T) {
	var calls atomic.Int32

	h := azureSidecarTestHandler(t, sidecarTestOrigin(t, "data", `"v"`, &calls))
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			// Azure.Core ClientRequestIdPolicy sends both correlation headers.
			headers := http.Header{}
			headers.Set("x-ms-client-request-id", "8c5bfe95-d0ce-4d9a-b82a-2a45094c71ae")
			headers.Set("x-ms-return-client-request-id", "true")
			headers.Set("x-ms-version", "2026-02-06")
			headers.Set("User-Agent", "azsdk-net-Storage.Blobs/12.25.1 (.NET 8.0.0; Linux)")
			w := sidecarTestRequest(h, method, "/bucket/blob", headers)

			wantBody := "data"
			if method == "HEAD" {
				wantBody = ""
			}

			if w.Code != 200 || w.Body.String() != wantBody || w.Header().Get("Content-Length") != "4" {
				t.Fatalf("SDK-style response: %d %v %q", w.Code, w.Header(), w.Body.String())
			}

			for _, name := range []string{"x-ms-lease-id", "x-ms-encryption-key", "x-ms-if-tags", "x-ms-range-get-content-md5", "x-ms-range-get-content-crc64", "x-ms-unknown"} {
				t.Run(name, func(t *testing.T) {
					semanticHeaders := headers.Clone()
					semanticHeaders.Set(name, "required")

					before := calls.Load()
					w := sidecarTestRequest(h, method, "/bucket/blob", semanticHeaders)
					assertAzureSidecarError(t, w, method, 501, "NotImplemented")

					if calls.Load() != before {
						t.Fatal("unsupported requirement reached Racer")
					}
				})
			}
		})
	}
}

func TestAzureSidecarPrecommitFailures(t *testing.T) {
	for _, mode := range []string{"pin", "size", "type"} {
		t.Run(mode, func(t *testing.T) {
			m := sidecarTestMetadata(t, 4, `"v"`)
			h := azureSidecarTestHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				if r.Head {
					return m, nil, nil
				}

				if r.ETag != m.ETag {
					t.Error("missing Stat pin")
				}

				changed := m

				switch mode {
				case "pin":
					return racersdk.Metadata{}, nil, racersdk.ErrVersionMismatch
				case "size":
					changed.Size++
				case "type":
					changed.ContentType = "application/octet-stream"
				}

				return changed, io.NopCloser(strings.NewReader(strings.Repeat("x", int(changed.Size)))), nil
			})

			status, code := 502, "BadGateway"
			if mode == "pin" {
				status, code = 412, "ConditionNotMet"
			}

			assertAzureSidecarError(t, sidecarTestRequest(h, "GET", "/bucket/blob", nil), "GET", status, code)
		})
	}
}

func TestAzureSidecarSDKMissingBlob(t *testing.T) {
	h := azureSidecarTestHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, racersdk.ErrNotFound
	})
	server := httptest.NewServer(h)
	t.Cleanup(server.Close)

	client, err := blob.NewClientWithNoCredential(server.URL+"/bucket/missing", &blob.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: server.Client(), Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, head := range []bool{false, true} {
		if head {
			_, err = client.GetProperties(t.Context(), nil)
		} else {
			_, err = client.DownloadStream(t.Context(), nil)
		}

		var responseError *azcore.ResponseError
		if !errors.As(err, &responseError) || responseError.ErrorCode != "BlobNotFound" || responseError.StatusCode != 404 {
			t.Fatalf("SDK missing blob error (HEAD=%v): %v", head, err)
		}
	}
}
