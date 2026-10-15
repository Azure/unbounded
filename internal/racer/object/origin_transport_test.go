// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/Azure/unbounded/pkg/racersdk"
)

type originConnectionError bool

func (e originConnectionError) Error() string         { return "private connection failure" }
func (e originConnectionError) ConnectionError() bool { return bool(e) }

func TestOriginSendErrorPrecedence(t *testing.T) {
	send := &smithyhttp.RequestSendError{Err: io.EOF}
	for _, tc := range []struct {
		name   string
		status int
		cause  error
		want   error
	}{
		{"send", 0, send, racersdk.ErrUnavailable},
		{"connection", 0, originConnectionError(true), racersdk.ErrUnavailable},
		{"not-connection", 0, originConnectionError(false), errInvalidS3Response},
		{"unmarked-eof", 0, io.EOF, errInvalidS3Response},
		{"configuration", 0, errors.New("private configuration failure"), errInvalidS3Response},
		{"canceled", 0, &smithyhttp.RequestSendError{Err: context.Canceled}, context.Canceled},
		{"deadline", 0, &smithyhttp.RequestSendError{Err: context.DeadlineExceeded}, context.DeadlineExceeded},
		{"canceled-api-status", 403, errors.Join(context.Canceled, send, &smithy.GenericAPIError{Code: "NoSuchKey"}), context.Canceled},
		{"deadline-api-status", 403, errors.Join(context.DeadlineExceeded, send, &smithy.GenericAPIError{Code: "NoSuchKey"}), context.DeadlineExceeded},
		{"known-api", 0, errors.Join(send, &smithy.GenericAPIError{Code: "AccessDenied"}), racersdk.ErrForbidden},
		{"known-api-status", 403, errors.Join(send, &smithy.GenericAPIError{Code: "NoSuchKey"}), racersdk.ErrNotFound},
		{"unknown-api", 0, errors.Join(send, &smithy.GenericAPIError{Code: "Unknown"}), racersdk.ErrUnavailable},
		{"unauthorized", 401, send, racersdk.ErrUnauthorized},
		{"forbidden", 403, send, racersdk.ErrForbidden},
		{"not-found", 404, send, racersdk.ErrNotFound},
		{"delete-marker-version", 405, send, racersdk.ErrNotFound},
		{"not-acceptable", 406, send, errInvalidS3Response},
		{"precondition", 412, send, racersdk.ErrVersionMismatch},
		{"invalid-range", 416, send, errInvalidS3Response},
		{"server-error", 500, send, racersdk.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &smithyhttp.ResponseError{
				Response: &smithyhttp.Response{Response: &http.Response{StatusCode: tc.status}},
				Err:      tc.cause,
			}
			assertOriginTransportClassification(t, upstream, tc.cause, tc.want)
		})
	}
}

func assertOriginTransportClassification(t *testing.T, upstream, cause, want error) {
	t.Helper()

	for _, pinned := range []bool{false, true} {
		classification := want
		if pinned && classification == racersdk.ErrNotFound {
			classification = racersdk.ErrVersionMismatch
		}

		err := classifyS3Error(upstream, pinned)
		if !errors.Is(err, classification) || !errors.Is(err, upstream) || !errors.Is(err, cause) {
			t.Fatalf("classification or cause lost: %v, want %v", err, classification)
		}

		if got, want := err.Error(), classification.Error()+": S3 request failed"; got != want {
			t.Fatalf("upstream details not redacted: %q, want %q", got, want)
		}

		if classification == racersdk.ErrUnavailable && sidecarSDKFailure(err) != http.StatusServiceUnavailable {
			t.Fatal("transport failure did not map to HTTP 503")
		}

		if classification == errInvalidS3Response && sidecarSDKFailure(err) != http.StatusBadGateway {
			t.Fatal("nontransport failure did not map to HTTP 502")
		}
	}
}

func TestOriginAWSClientTransportFailures(t *testing.T) {
	for _, mode := range []string{"eof", "tls", "refused", "missing-region", "credentials"} {
		t.Run(mode, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode != "eof" {
					t.Error("unexpected request reached S3")
					w.WriteHeader(http.StatusInternalServerError)

					return
				}

				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}

				if err := conn.Close(); err != nil {
					t.Error(err)
				}
			})

			var server *httptest.Server
			if mode == "tls" {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}

			t.Cleanup(server.Close)

			if mode == "refused" {
				server.Close()
			}

			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			t.Cleanup(transport.CloseIdleConnections)

			credentialFailure := errors.New("private credential configuration failure")

			options := s3.Options{
				Region: "us-east-1", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
				Credentials:      credentials.NewStaticCredentialsProvider("placeholder", "placeholder", ""),
				RetryMaxAttempts: 1, HTTPClient: &http.Client{Transport: transport, Timeout: 5 * time.Second},
			}
			if mode == "missing-region" {
				options.Region = ""
			}

			if mode == "credentials" {
				options.Credentials = aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{}, credentialFailure
				})
			}

			client := s3.New(options)

			for _, operation := range []string{"HEAD", "GET"} {
				t.Run(operation, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					defer cancel()

					var upstream error
					if operation == "HEAD" {
						_, upstream = client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("bucket"), Key: aws.String("private-key")})
					} else {
						_, upstream = client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("bucket"), Key: aws.String("private-key")})
					}

					if upstream == nil {
						t.Fatal("expected real AWS SDK failure")
					}

					want, cause := racersdk.ErrUnavailable, upstream

					var send *smithyhttp.RequestSendError

					if mode == "missing-region" || mode == "credentials" {
						want = errInvalidS3Response

						if errors.As(upstream, &send) {
							t.Fatal("configuration failure reached transport", upstream)
						}
					} else {
						var status interface{ HTTPStatusCode() int }
						if !errors.As(upstream, &status) || status.HTTPStatusCode() != 0 || !errors.As(upstream, &send) {
							t.Fatal("expected SDK status-zero response wrapping RequestSendError", upstream)
						}

						cause = send.Err
					}

					switch mode {
					case "eof":
						cause = io.EOF
					case "refused":
						cause = syscall.ECONNREFUSED
					case "tls":
						var certificate x509.UnknownAuthorityError
						if !errors.As(upstream, &certificate) {
							t.Fatal("expected certificate verification failure", upstream)
						}
					case "credentials":
						cause = credentialFailure
					}

					assertOriginTransportClassification(t, upstream, cause, want)
				})
			}
		})
	}
}
