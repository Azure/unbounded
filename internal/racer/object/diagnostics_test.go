// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Azure/unbounded/pkg/racersdk"
)

func TestInvalidResponseCategory(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"redirect-301", classifyS3Error(originStatusError{301}, false), "redirect"},
		{"redirect-307", classifyS3Error(originStatusError{307}, false), "redirect"},
		{"status-400", classifyS3Error(originStatusError{400}, false), "status"},
		{"status-416", classifyS3Error(originStatusError{416}, false), "status"},
		{"validation", errS3Validation, "validation"},
		{"unknown", classifyS3Error(errors.New("private"), false), "upstream"},
		{"not-invalid", classifyS3Error(originStatusError{404}, false), ""},
		{"delete-marker-405", classifyS3Error(originStatusError{405}, false), ""},
		{"nil", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := InvalidResponseCategory(tc.err); got != tc.want {
				t.Fatalf("category = %q, want %q", got, tc.want)
			}

			if tc.want != "" && (!errors.Is(tc.err, errInvalidS3Response) || sidecarSDKFailure(tc.err) != http.StatusBadGateway) {
				t.Fatal("sub-category changed the error mapping")
			}
		})
	}

	// A delete marker's 405 is not found, which diagnostics log as notfound.
	if err := classifyS3Error(originStatusError{405}, false); !errors.Is(err, racersdk.ErrNotFound) || errors.Is(err, errInvalidS3Response) {
		t.Fatalf("405 = %v, want not found", err)
	}

	if errors.Is(errS3Status, racersdk.ErrUnavailable) {
		t.Fatal("status sub-category matched an unrelated sentinel")
	}
}

func TestInvalidHeadIsValidation(t *testing.T) {
	_, err := (&objectOrigin{}).headMetadata(nil, Object{})
	if InvalidResponseCategory(err) != "validation" {
		t.Fatalf("head validation category = %q", InvalidResponseCategory(err))
	}
}

func sidecarReportingHandler(t *testing.T, origin racersdk.Origin, reports *[]SidecarFailure) http.Handler {
	t.Helper()

	h, err := NewSidecar(testSDKClient(t, origin), SidecarConfig{
		Namespace: "test", Buckets: []string{"my-bucket"},
		OnFailure: func(f SidecarFailure) { *reports = append(*reports, f) },
	})
	if err != nil {
		t.Fatal(err)
	}

	return h
}

func TestSidecarReportsServerFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		report bool
		want   error
	}{
		// Only Racer wire errors cross to the sidecar; S3 detail stays in origin logs.
		{"unavailable", racersdk.ErrUnavailable, 503, true, racersdk.ErrUnavailable},
		{"version", racersdk.ErrVersionMismatch, 503, true, racersdk.ErrVersionMismatch},
		{"invalid", errS3Redirect, 502, true, nil},
		{"notfound", racersdk.ErrNotFound, 404, false, nil},
		{"forbidden", racersdk.ErrForbidden, 403, false, nil},
		{"bad-request", racersdk.ErrInvalidRequest, 400, false, nil},
	} {
		for _, method := range []string{"GET", "HEAD"} {
			t.Run(tc.name+method, func(t *testing.T) {
				var reports []SidecarFailure

				h := sidecarReportingHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
					return racersdk.Metadata{}, nil, fmt.Errorf("private: %w", tc.err)
				}, &reports)

				w := sidecarTestRequest(h, method, "/my-bucket/secret-key?versionId=v", nil)
				if w.Code != tc.status {
					t.Fatalf("status %d want %d", w.Code, tc.status)
				}

				if !tc.report {
					if len(reports) != 0 {
						t.Fatalf("client error reported: %+v", reports)
					}

					return
				}

				if len(reports) != 1 {
					t.Fatalf("reports = %+v", reports)
				}

				got := reports[0]
				if got.Method != method || got.Status != tc.status || got.Bucket != "my-bucket" || got.Err == nil ||
					(tc.want != nil && !errors.Is(got.Err, tc.want)) {
					t.Fatalf("report = %+v", got)
				}
			})
		}
	}
}

func TestSidecarReportsMetadataMismatch(t *testing.T) {
	var (
		reports []SidecarFailure
		calls   atomic.Int32
	)

	m := sidecarTestMetadata(t, 4, `"same"`)
	h := sidecarReportingHandler(t, func(_ context.Context, r racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if calls.Add(1) == 1 {
			return m, nil, nil
		}

		changed := m
		changed.ContentType = "application/octet-stream"

		if r.Head {
			return changed, nil, nil
		}

		return changed, io.NopCloser(strings.NewReader("xxxx")), nil
	}, &reports)

	// A suffix range takes the Stat path, whose read is pinned and checked.
	w := sidecarTestRequest(h, "GET", "/my-bucket/key", http.Header{"Range": {"bytes=-4"}})
	if w.Code != http.StatusBadGateway || len(reports) != 1 || !errors.Is(reports[0].Err, ErrMetadataMismatch) || reports[0].Bucket != "my-bucket" {
		t.Fatalf("status %d reports %+v", w.Code, reports)
	}
}

func TestSidecarNilOnFailure(t *testing.T) {
	h, _ := sidecarTestHandler(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{}, nil, racersdk.ErrUnavailable
	})

	if w := sidecarTestRequest(h, "GET", "/bucket/key", nil); w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
}

func TestSidecarReportsCanceledRequest(t *testing.T) {
	var reports []SidecarFailure

	h := sidecarReportingHandler(t, sidecarTestOrigin(t, "data", `"tag"`, nil), &reports)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/my-bucket/key", nil).WithContext(ctx))

	if w.Code != 503 || len(reports) != 1 || !errors.Is(reports[0].Err, context.Canceled) {
		t.Fatalf("status %d reports %+v", w.Code, reports)
	}
}
