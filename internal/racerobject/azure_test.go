// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racerobject

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"

	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

const azureTestTag = `"azure-version"`

func azureTestRequest(t *testing.T, key, version string) racersdk.Request {
	t.Helper()

	request, err := NewRequest("azure-store", "container", key, version)
	if err != nil {
		t.Fatal(err)
	}

	return request
}

func azureTestClient(t *testing.T, handler http.HandlerFunc, transport func(*http.Client) policy.Transporter) *service.Client {
	t.Helper()

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	var sender policy.Transporter = server.Client()
	if transport != nil {
		sender = transport(server.Client())
	}

	client, err := service.NewClientWithNoCredential(server.URL+"/account", &service.ClientOptions{
		ClientOptions: azcore.ClientOptions{Transport: sender, Retry: policy.RetryOptions{MaxRetries: -1}},
	})
	if err != nil {
		t.Fatal(err)
	}

	return client
}

func azureTestOrigin(t *testing.T, client *service.Client, config OriginConfig) racersdk.Origin {
	t.Helper()

	origin, err := NewAzureOrigin(client, config)
	if err != nil {
		t.Fatal(err)
	}

	return origin
}

func azureTestHeaders(h http.Header, size int64, version string) {
	h.Set("ETag", azureTestTag)
	h.Set("Content-Length", strconv.FormatInt(size, 10))
	h.Set("Content-Type", "application/test")
	h.Set("x-ms-blob-type", "BlockBlob")

	if version != "" {
		h.Set("x-ms-version-id", version)
	}
}

func azureTestRead(t *testing.T, client *racersdk.Client, request racersdk.Request, options ...racersdk.ReadOptions) ([]byte, error) {
	t.Helper()

	value, err := client.Get(t.Context(), request, options...)
	if err != nil {
		return nil, err
	}
	defer value.Close()

	return io.ReadAll(value)
}

func azureAssertError(t *testing.T, err, want error) {
	t.Helper()

	if want == errInvalidAzureResponse {
		if err == nil || sidecarSDKFailure(err) != http.StatusBadGateway {
			t.Fatalf("error = %v, want invalid upstream response", err)
		}

		return
	}

	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func TestAzureOriginBridge(t *testing.T) {
	for _, version := range []string{"", "2026-10-13T00:00:00.0000000Z+/=%"} {
		for _, data := range []string{"", "hello"} {
			t.Run(version+"/"+data, func(t *testing.T) {
				var heads, gets atomic.Int32

				client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/account/container/a/../exact%2F +雪" || r.URL.Query().Get("versionid") != version ||
						r.URL.Query().Has("versionid") != (version != "") {
						t.Errorf("changed identity: %s", r.URL)
					}

					azureTestHeaders(w.Header(), int64(len(data)), version)

					switch r.Method {
					case http.MethodHead:
						heads.Add(1)
					case http.MethodGet:
						gets.Add(1)

						if data == "" || r.Header.Get("x-ms-range") != "bytes=0-4" || r.Header.Get("If-Match") != azureTestTag {
							t.Errorf("invalid GET: %v", r.Header)
						}

						w.Header().Set("Content-Range", "bytes 0-4/5")
						w.WriteHeader(http.StatusPartialContent)
						_, _ = io.WriteString(w, data)
					default:
						t.Errorf("unexpected method %s", r.Method)
					}
				}, nil)
				origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store", MetadataTTL: 30 * time.Second})
				sdk := racersdktest.NewClient(t, origin)
				request := azureTestRequest(t, "a/../exact%2F +雪", version)
				before := time.Now()

				metadata, err := sdk.Stat(t.Context(), request)
				if err != nil || metadata.Size != int64(len(data)) || metadata.ETag != azureTestTag || metadata.ContentType != "application/test" ||
					metadata.ExpiresAt.Before(before.Add(29*time.Second)) || metadata.ExpiresAt.Nanosecond()%int(time.Millisecond) != 0 {
					t.Fatalf("metadata = %+v, error = %v", metadata, err)
				}

				got, err := azureTestRead(t, sdk, request)
				if err != nil || string(got) != data || heads.Load() != 2 || gets.Load() != int32(min(len(data), 1)) {
					t.Fatalf("read = %q, error = %v, calls = %d/%d", got, err, heads.Load(), gets.Load())
				}
			})
		}
	}
}

func TestAzureOriginExactKeys(t *testing.T) {
	for _, key := range []string{".", "..", "../", "/", "/leading", "trailing/", "a//b/../c", "a%2Fb", "a+b c", "雪/☃", "a?b#c", "a\x7fb", "a\x00b", "line\nbreak"} {
		t.Run(key, func(t *testing.T) {
			client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/account/container/"+key {
					t.Errorf("path = %q, want key %q", r.URL.Path, key)
				}

				if r.URL.Query().Get("versionid") != "v+/%&other=value" {
					t.Errorf("changed version: %s", r.URL)
				}

				azureTestHeaders(w.Header(), 0, "v+/%&other=value")
			}, nil)
			origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"})

			_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: azureTestRequest(t, key, "v+/%&other=value"), Head: true})
			if err != nil || body != nil {
				t.Fatal(body, err)
			}
		})
	}
}

func TestAzureOriginConfigAndScope(t *testing.T) {
	client := azureTestClient(t, func(w http.ResponseWriter, _ *http.Request) { azureTestHeaders(w.Header(), 0, "") }, nil)
	for _, config := range []OriginConfig{{}, {Namespace: "bad space"}, {Namespace: "azure-store", MetadataTTL: -1}, {Namespace: "azure-store", Buckets: []string{""}}, {Namespace: "azure-store", Buckets: []string{"https://evil"}}} {
		_, err := NewAzureOrigin(client, config)
		azureAssertError(t, err, racersdk.ErrInvalidRequest)
	}

	_, err := NewAzureOrigin(nil, OriginConfig{Namespace: "azure-store"})
	azureAssertError(t, err, racersdk.ErrInvalidRequest)

	buckets := []string{"container"}
	origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store", Buckets: buckets})
	buckets[0] = "changed"
	request := azureTestRequest(t, "key", "")

	m, _, err := origin(t.Context(), racersdk.OriginRequest{Request: request, Head: true})
	if err != nil || m.ExpiresAt.After(time.Now()) || m.ExpiresAt.Before(time.Now().Add(-time.Second)) {
		t.Fatal("allowlist was not copied or zero TTL changed", m, err)
	}

	for _, mode := range []string{"namespace", "container", "hash", "noncanonical"} {
		t.Run(mode, func(t *testing.T) {
			client := azureTestClient(t, func(http.ResponseWriter, *http.Request) { t.Error("invalid request reached Azure") }, nil)
			config := OriginConfig{Namespace: "azure-store"}
			request := azureTestRequest(t, "key", "")
			want := racersdk.ErrForbidden

			switch mode {
			case "namespace":
				config.Namespace = "other"
			case "container":
				config.Buckets = []string{"other"}
			case "hash":
				request.Key[0] ^= 1
				want = racersdk.ErrInvalidRequest
			case "noncanonical":
				request.Metadata += " "
				request.Key = sha256.Sum256([]byte(request.Metadata))
				want = racersdk.ErrInvalidRequest
			}

			origin := azureTestOrigin(t, client, config)
			_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: request, Head: true})
			azureAssertError(t, err, want)

			if body != nil {
				t.Fatal("unexpected body")
			}
		})
	}
}

func TestAzureOriginHeadValidation(t *testing.T) {
	for _, tc := range []struct{ name, header, value string }{
		{"missing-length", "Content-Length", ""},
		{"negative-length", "Content-Length", "-1"},
		{"bad-length", "Content-Length", "bad"},
		{"missing-tag", "ETag", ""},
		{"weak-tag", "ETag", `W/"weak"`},
		{"bare-tag", "ETag", "bare"},
		{"space-tag", "ETag", `"bad tag"`},
		{"missing-type", "x-ms-blob-type", ""},
		{"append", "x-ms-blob-type", "AppendBlob"},
		{"page", "x-ms-blob-type", "PageBlob"},
		{"missing-version", "x-ms-version-id", ""},
		{"wrong-version", "x-ms-version-id", "wrong"},
		{"gzip", "Content-Encoding", "gzip"},
		{"br", "Content-Encoding", "br"},
		{"archived", "x-ms-access-tier", "Archive"},
		{"rehydrating", "x-ms-archive-status", "rehydrate-pending-to-hot"},
		{"tag-count", "x-ms-tag-count", "invalid"},
		{"last-modified", "Last-Modified", "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodHead {
					t.Error("invalid HEAD reached GET")
				}

				azureTestHeaders(w.Header(), 5, "requested")
				w.Header().Set(tc.header, tc.value)
				w.WriteHeader(http.StatusOK)
			}, nil)
			sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))
			_, err := sdk.Stat(t.Context(), azureTestRequest(t, "key", "requested"))
			azureAssertError(t, err, errInvalidAzureResponse)
		})
	}
}

type azureTestTransport func(*http.Request) (*http.Response, error)

func (f azureTestTransport) Do(r *http.Request) (*http.Response, error) { return f(r) }

type azureTestBody struct {
	io.ReadCloser
	closes *atomic.Int32
}

func (b *azureTestBody) Close() error {
	b.closes.Add(1)
	return b.ReadCloser.Close()
}

func TestAzureOriginPageValidationAndCleanup(t *testing.T) {
	for _, tc := range []struct{ name, header, value string }{
		{"length", "Content-Length", "4"},
		{"missing-length", "Content-Length", ""},
		{"missing-range", "Content-Range", ""},
		{"range", "Content-Range", "bytes 1-5/5"},
		{"total", "Content-Range", "bytes 0-4/6"},
		{"tag", "ETag", `"changed"`},
		{"missing-tag", "ETag", ""},
		{"weak-tag", "ETag", `W/"weak"`},
		{"content-type", "Content-Type", "text/plain"},
		{"version", "x-ms-version-id", "changed"},
		{"missing-version", "x-ms-version-id", ""},
		{"encoding", "Content-Encoding", "gzip"},
		{"missing-type", "x-ms-blob-type", ""},
		{"append", "x-ms-blob-type", "AppendBlob"},
		{"page", "x-ms-blob-type", "PageBlob"},
		{"short", "", ""},
		{"long", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32

			client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				azureTestHeaders(w.Header(), 5, "version")

				if r.Method == http.MethodGet {
					w.Header().Set("Content-Range", "bytes 0-4/5")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = io.WriteString(w, "hello")
				}
			}, func(client *http.Client) policy.Transporter {
				return azureTestTransport(func(r *http.Request) (*http.Response, error) {
					response, err := client.Do(r)
					if err == nil && r.Method == http.MethodGet {
						if tc.header != "" {
							response.Header.Set(tc.header, tc.value)
						}

						if tc.name == "short" || tc.name == "long" {
							_ = response.Body.Close()

							data := "hell"
							if tc.name == "long" {
								data = "hello!"
							}

							response.Body = io.NopCloser(strings.NewReader(data))
						}

						response.Body = &azureTestBody{ReadCloser: response.Body, closes: &closes}
					}

					return response, err
				})
			})
			sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))
			got, err := azureTestRead(t, sdk, azureTestRequest(t, "key", "version"))

			want := errInvalidAzureResponse
			if tc.name == "tag" {
				want = racersdk.ErrVersionMismatch
			}

			if tc.name == "short" || tc.name == "long" {
				if err == nil {
					t.Fatal("invalid byte count accepted")
				}
			} else {
				azureAssertError(t, err, want)
			}

			if len(got) != 0 {
				t.Fatalf("invalid page exposed bytes: %q", got)
			}

			deadline := time.Now().Add(5 * time.Second)
			for closes.Load() == 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}

			if closes.Load() != 1 {
				t.Fatalf("body closed %d times", closes.Load())
			}
		})
	}
}

func TestAzureOriginHeaderDecodeBodyCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, header string
		status       int
		want         error
	}{
		{"tag-count", "x-ms-tag-count", http.StatusPartialContent, errInvalidAzureResponse},
		{"last-modified", "Last-Modified", http.StatusPartialContent, errInvalidAzureResponse},
		{"boolean", "x-ms-server-encrypted", http.StatusPartialContent, errInvalidAzureResponse},
		{"base64", "Content-MD5", http.StatusPartialContent, errInvalidAzureResponse},
		{"full-response", "x-ms-tag-count", http.StatusOK, errInvalidAzureResponse},
		{"consumed-error", "x-ms-tag-count", http.StatusForbidden, racersdk.ErrForbidden},
	} {
		for _, bridge := range []bool{false, true} {
			t.Run(fmt.Sprint(tc.name, "/bridge=", bridge), func(t *testing.T) {
				var closes atomic.Int32

				client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
					azureTestHeaders(w.Header(), 5, "")

					if r.Method == http.MethodGet {
						w.Header().Set(tc.header, "private malformed header")
						w.Header().Set("Content-Range", "bytes 0-4/5")
						w.WriteHeader(tc.status)
						_, _ = io.WriteString(w, "hello")
					}
				}, func(client *http.Client) policy.Transporter {
					return azureTestTransport(func(r *http.Request) (*http.Response, error) {
						response, err := client.Do(r)
						if err == nil && r.Method == http.MethodGet {
							response.Body = &azureTestBody{ReadCloser: response.Body, closes: &closes}
						}

						return response, err
					})
				})
				origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"})

				request := azureTestRequest(t, "key", "")
				if bridge {
					sdk := racersdktest.NewClient(t, origin)
					got, err := azureTestRead(t, sdk, request)
					azureAssertError(t, err, tc.want)

					if len(got) != 0 {
						t.Fatalf("rejected page exposed bytes: %q", got)
					}

					if err := sdk.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: request, Length: racersdk.PageSize})
					azureAssertError(t, err, tc.want)

					if strings.Contains(err.Error(), "private") {
						t.Fatal("leaked header details", err)
					}

					if tc.status == http.StatusForbidden {
						if body != nil {
							_ = body.Close()

							t.Fatal("returned an already consumed error body")
						}
					} else {
						if body == nil {
							t.Fatal("lost raw body on header decode failure")
						}

						beforeClose := closes.Load()
						got, err := io.ReadAll(body)
						closeErr := body.Close()

						if beforeClose != 0 {
							t.Fatal("transferred an already closed body")
						}

						if err != nil || string(got) != "hello" {
							t.Fatal("transferred body was consumed", string(got), err)
						}

						if closeErr != nil {
							t.Fatal(closeErr)
						}
					}
				}

				if closes.Load() != 1 {
					t.Fatalf("body closed %d times, want exactly once", closes.Load())
				}
			})
		}
	}
}

func TestAzureOriginPageBounds(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		size, offset, length int64
		want                 error
	}{
		{"empty", 0, 0, racersdk.PageSize, nil},
		{"outside", 5, racersdk.PageSize, racersdk.PageSize, nil},
		{"at-end", racersdk.PageSize, racersdk.PageSize, racersdk.PageSize, nil},
		{"negative", 5, -racersdk.PageSize, racersdk.PageSize, racersdk.ErrInvalidRequest},
		{"unaligned", 5, 1, racersdk.PageSize, racersdk.ErrInvalidRequest},
		{"zero", 5, 0, 0, racersdk.ErrInvalidRequest},
		{"negative-length", 5, 0, -1, racersdk.ErrInvalidRequest},
		{"oversize", 5, 0, racersdk.PageSize + 1, racersdk.ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin := &azureOrigin{}

			body, err := origin.readPage(t.Context(), racersdk.OriginRequest{Offset: tc.offset, Length: tc.length}, nil, blob.GetPropertiesResponse{}, racersdk.Metadata{Size: tc.size})
			if body != nil || !errors.Is(err, tc.want) {
				t.Fatal(body, err)
			}
		})
	}

	for _, tc := range []struct{ size, offset, length int64 }{
		{racersdk.PageSize + 3, racersdk.PageSize, racersdk.PageSize},
		{math.MaxInt64, math.MaxInt64 / racersdk.PageSize * racersdk.PageSize, racersdk.PageSize},
		{5, 0, 2},
	} {
		t.Run(fmt.Sprint(tc.size, tc.length), func(t *testing.T) {
			count := min(tc.length, tc.size-tc.offset)
			client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				azureTestHeaders(w.Header(), tc.size, "")

				if r.Method == http.MethodGet {
					if r.Header.Get("x-ms-range") != fmt.Sprintf("bytes=%d-%d", tc.offset, tc.offset+count-1) || r.Header.Get("If-Match") != azureTestTag {
						t.Errorf("range/pin: %v", r.Header)
					}

					w.Header().Set("Content-Length", strconv.FormatInt(count, 10))
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", tc.offset, tc.offset+count-1, tc.size))
					w.WriteHeader(http.StatusPartialContent)
				}
			}, nil)
			origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"})

			_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: azureTestRequest(t, "key", ""), Offset: tc.offset, Length: tc.length})
			if err != nil || body == nil {
				t.Fatal(body, err)
			}

			_ = body.Close()
		})
	}
}

func TestAzureOriginPinsAndUpstreamErrors(t *testing.T) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		for _, tc := range []struct {
			status int
			code   string
			want   error
		}{
			{404, "BlobNotFound", racersdk.ErrNotFound},
			{404, "ContainerNotFound", racersdk.ErrNotFound},
			{401, "", racersdk.ErrUnauthorized},
			{403, "AuthorizationPermissionMismatch", racersdk.ErrForbidden},
			{412, "ConditionNotMet", racersdk.ErrVersionMismatch},
			{408, "", racersdk.ErrUnavailable},
			{429, "", racersdk.ErrUnavailable},
			{500, "InternalError", racersdk.ErrUnavailable},
			{503, "ServerBusy", racersdk.ErrUnavailable},
			{416, "InvalidRange", errInvalidAzureResponse},
			{409, "BlobArchived", errInvalidAzureResponse},
			{400, "Unknown", errInvalidAzureResponse},
		} {
			for _, pin := range []string{"", azureTestTag} {
				t.Run(fmt.Sprint(method, tc.status, tc.code, pin), func(t *testing.T) {
					client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
						wantPin := pin
						if r.Method == http.MethodGet {
							wantPin = azureTestTag
						}

						if r.Header.Get("If-Match") != wantPin {
							t.Error("missing pin")
						}

						if r.Method == method {
							w.Header().Set("x-ms-error-code", tc.code)
							w.WriteHeader(tc.status)
							_, _ = io.WriteString(w, "private credential and URL")

							return
						}

						azureTestHeaders(w.Header(), 5, "")
					}, nil)
					origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"})
					_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: azureTestRequest(t, "key", ""), Length: racersdk.PageSize, ETag: pin})

					want := tc.want
					if want == racersdk.ErrNotFound && (pin != "" || method == http.MethodGet) {
						want = racersdk.ErrVersionMismatch
					}

					azureAssertError(t, err, want)

					if body != nil || strings.Contains(err.Error(), "private") {
						t.Fatal("unexpected body or leaked details", body, err)
					}
				})
			}
		}
	}

	t.Run("ignored-head-pin", func(t *testing.T) {
		client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodHead || r.Header.Get("If-Match") != `"old"` {
				t.Error("wrong request after pin mismatch")
			}

			azureTestHeaders(w.Header(), 5, "")
		}, nil)
		origin := azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"})
		_, body, err := origin(t.Context(), racersdk.OriginRequest{Request: azureTestRequest(t, "key", ""), Length: racersdk.PageSize, ETag: `"old"`})
		azureAssertError(t, err, racersdk.ErrVersionMismatch)

		if body != nil {
			t.Fatal("unexpected body")
		}
	})
}

func TestAzureErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		cause error
		want  error
	}{
		{context.Canceled, context.Canceled},
		{context.DeadlineExceeded, context.DeadlineExceeded},
		{errors.New("private credential"), errInvalidAzureResponse},
		{&net.DNSError{Err: "private URL"}, racersdk.ErrUnavailable},
		{net.ErrClosed, racersdk.ErrUnavailable},
		{&url.Error{Op: "Get", URL: "private", Err: &net.OpError{Op: "dial", Err: errors.New("connection failed")}}, racersdk.ErrUnavailable},
		{&url.Error{Op: "Get", URL: "private", Err: errors.New("malformed HTTP response")}, errInvalidAzureResponse},
		{io.EOF, racersdk.ErrUnavailable},
		{io.ErrUnexpectedEOF, racersdk.ErrUnavailable},
		{&azidentity.AuthenticationFailedError{}, racersdk.ErrUnauthorized},
		{&azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: 503}}, racersdk.ErrUnavailable},
		{&azidentity.AuthenticationRequiredError{}, racersdk.ErrUnauthorized},
		{&azcore.ResponseError{ErrorCode: "AuthenticationFailed", StatusCode: 404}, racersdk.ErrUnauthorized},
		{&azcore.ResponseError{ErrorCode: "AuthorizationFailure", StatusCode: 404}, racersdk.ErrForbidden},
		{&azcore.ResponseError{ErrorCode: "ResourceNotFound", StatusCode: 500}, racersdk.ErrNotFound},
		{&azcore.ResponseError{ErrorCode: "OperationTimedOut", StatusCode: 400}, racersdk.ErrUnavailable},
		{&azcore.ResponseError{ErrorCode: "Unknown", StatusCode: 404}, racersdk.ErrNotFound},
		{&azcore.ResponseError{ErrorCode: "BlobBeingRehydrated", StatusCode: 503}, errInvalidAzureResponse},
		{errors.Join(context.Canceled, &azcore.ResponseError{StatusCode: 404}), context.Canceled},
		{errors.Join(context.DeadlineExceeded, &azidentity.AuthenticationFailedError{}), context.DeadlineExceeded},
	} {
		for _, pinned := range []bool{false, true} {
			want := tc.want
			if pinned && want == racersdk.ErrNotFound {
				want = racersdk.ErrVersionMismatch
			}

			err := classifyAzureError(fmt.Errorf("private URL: %w", tc.cause), pinned)
			azureAssertError(t, err, want)

			if !errors.Is(err, tc.cause) || strings.Contains(err.Error(), "private") {
				t.Fatal("lost cause or leaked details", err)
			}
		}
	}
}

func TestAzureOriginCancellation(t *testing.T) {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			bodyReady := make(chan struct{})

			var closes atomic.Int32

			client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				azureTestHeaders(w.Header(), 5, "")

				if r.Method != method {
					return
				}

				if method == http.MethodGet {
					w.Header().Set("Content-Range", "bytes 0-4/5")
					w.WriteHeader(http.StatusPartialContent)
					w.(http.Flusher).Flush()
				}

				close(entered)
				<-r.Context().Done()
				close(canceled)
			}, func(client *http.Client) policy.Transporter {
				return azureTestTransport(func(r *http.Request) (*http.Response, error) {
					response, err := client.Do(r)
					if err == nil && r.Method == http.MethodGet {
						response.Body = &azureTestBody{ReadCloser: response.Body, closes: &closes}

						close(bodyReady)
					}

					return response, err
				})
			})
			sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			done := make(chan error, 1)
			request := azureTestRequest(t, "key", "")

			go func() {
				if method == http.MethodHead {
					_, err := sdk.Stat(ctx, request)
					done <- err

					return
				}

				value, err := sdk.Get(ctx, request)
				if err == nil {
					_, err = io.ReadAll(value)
					_ = value.Close()
				}

				done <- err
			}()

			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}

			if method == http.MethodGet {
				select {
				case <-bodyReady:
				case <-time.After(5 * time.Second):
					t.Fatal("response body was not wrapped")
				}
			}

			cancel()

			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("upstream context was not canceled")
			}

			select {
			case err := <-done:
				if err == nil || method == http.MethodHead && !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation error", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SDK call did not stop")
			}

			if method == http.MethodGet {
				deadline := time.Now().Add(5 * time.Second)
				for closes.Load() == 0 && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}

				if closes.Load() != 1 {
					t.Fatal("body close count", closes.Load())
				}
			}
		})
	}

	for _, expired := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		want := context.Canceled

		if expired {
			cancel()
			ctx, cancel = context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		}

		cancel()

		_, body, err := (&azureOrigin{}).read(ctx, racersdk.OriginRequest{})
		azureAssertError(t, err, want)

		if body != nil {
			t.Fatal("canceled request returned body")
		}
	}
}

func TestAzureOriginIdentityEncoding(t *testing.T) {
	client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		azureTestHeaders(w.Header(), 5, "")
		w.Header().Set("Content-Encoding", "identity")

		if r.Method == http.MethodGet {
			w.Header().Set("Content-Range", "bytes 0-4/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "hello")
		}
	}, nil)
	sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))

	got, err := azureTestRead(t, sdk, azureTestRequest(t, "key", ""))
	if err != nil || string(got) != "hello" {
		t.Fatal(string(got), err)
	}

	head := blob.GetPropertiesResponse{ContentLength: to.Ptr(int64(-1)), BlobType: to.Ptr(blob.BlobTypeBlockBlob), ETag: to.Ptr(azcore.ETag(azureTestTag))}
	_, err = (&azureOrigin{}).headMetadata(head, Object{})
	azureAssertError(t, err, errInvalidAzureResponse)
}

func TestAzureOriginMultipage(t *testing.T) {
	var heads, gets atomic.Int32

	size := int64(racersdk.PageSize) + 3
	client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		azureTestHeaders(w.Header(), size, "version")

		if r.Method == http.MethodHead {
			if heads.Add(1) > 1 && r.Header.Get("If-Match") != azureTestTag {
				t.Error("continuation HEAD not pinned")
			}

			return
		}

		gets.Add(1)

		if r.Header.Get("If-Match") != azureTestTag {
			t.Error("GET not pinned")
		}

		if r.Header.Get("x-ms-range") == fmt.Sprintf("bytes=0-%d", racersdk.PageSize-1) {
			w.Header().Set("Content-Length", strconv.Itoa(racersdk.PageSize))
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", racersdk.PageSize-1, size))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.CopyN(w, originRepeatedByte('x'), racersdk.PageSize)

			return
		}

		if r.Header.Get("x-ms-range") != fmt.Sprintf("bytes=%d-%d", racersdk.PageSize, size-1) {
			t.Errorf("wrong final range: %v", r.Header)
		}

		w.Header().Set("Content-Length", "3")
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", racersdk.PageSize, size-1, size))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "end")
	}, nil)
	sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))

	value, err := sdk.Get(t.Context(), azureTestRequest(t, "key", "version"))
	if err != nil {
		t.Fatal(err)
	}
	defer value.Close()

	if n, err := io.CopyN(io.Discard, value, racersdk.PageSize); err != nil || n != racersdk.PageSize {
		t.Fatal("first page", n, err)
	}

	got, err := io.ReadAll(value)
	if err != nil || string(got) != "end" || heads.Load() != 2 || gets.Load() != 2 {
		t.Fatal("continuation", string(got), err, heads.Load(), gets.Load())
	}
}

func TestAzureOriginPinnedBridge(t *testing.T) {
	client := azureTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-Match") != azureTestTag {
			t.Errorf("missing pin on %s", r.Method)
		}

		azureTestHeaders(w.Header(), 5, "current-version")

		if r.Method == http.MethodGet {
			w.Header().Set("Content-Range", "bytes 0-4/5")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "hello")
		}
	}, nil)
	sdk := racersdktest.NewClient(t, azureTestOrigin(t, client, OriginConfig{Namespace: "azure-store"}))

	got, err := azureTestRead(t, sdk, azureTestRequest(t, "key", ""), racersdk.ReadOptions{ETag: azureTestTag})
	if err != nil || string(got) != "hello" {
		t.Fatal(string(got), err)
	}
}
