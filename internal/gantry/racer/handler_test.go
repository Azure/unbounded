// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/internal/gantry/mirror"
	"github.com/Azure/unbounded/internal/gantry/registryauth"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type contentBackendFunc func(http.ResponseWriter, *http.Request, ifaces.OriginRef)

func (f contentBackendFunc) ServeContent(w http.ResponseWriter, r *http.Request, ref ifaces.OriginRef) {
	f(w, r, ref)
}

type challengeOrigin struct {
	challenge string
	probes    atomic.Int32
}

func (*challengeOrigin) Pull(context.Context, ifaces.OriginRef) (io.ReadCloser, int64, error) {
	panic("Racer request reached legacy Pull")
}

func (*challengeOrigin) Head(context.Context, ifaces.OriginRef) (int64, string, error) {
	panic("Racer request reached legacy Head")
}

func (o *challengeOrigin) AuthenticationChallenge(context.Context, string) (string, bool, error) {
	o.probes.Add(1)
	return o.challenge, o.challenge != "", nil
}

func TestMirrorContentBackendDispatch(t *testing.T) {
	for _, tc := range []struct {
		path, rangeHeader, challenge string
		offset                       int64
		status                       int
	}{
		{"blobs/", "bytes=4-", "", 4, 502},
		{"blobs/", "bytes=0-", "", 0, 502},
		{"blobs/", "bytes=2-4", "", 0, 502},
		{"manifests/", "bytes=4-", "", 0, 502},
		{"blobs/", "bytes=4-", `Basic realm="registry"`, 0, 401},
	} {
		origin := &challengeOrigin{challenge: tc.challenge}
		calls := 0
		d := digestOf(nil)
		backend := contentBackendFunc(func(w http.ResponseWriter, _ *http.Request, ref ifaces.OriginRef) {
			calls++

			if ref.Offset != tc.offset || ref.Registry != "registry.example" || ref.Repository != "library/image" || ref.Digest != d || origin.probes.Load() != 1 {
				t.Fatalf("backend called before resolution/preflight: %+v", ref)
			}

			w.WriteHeader(http.StatusBadGateway)
		})
		server := mirror.New(testConfig(), nil, origin, mirror.WithContentBackend(backend))
		request := httptest.NewRequest(http.MethodGet, "/v2/library/image/"+tc.path+d.String(), nil)
		request.Header.Set("Range", tc.rangeHeader)

		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, request)

		wantCalls := 1
		if tc.challenge != "" {
			wantCalls = 0
		}

		if w.Code != tc.status || calls != wantCalls {
			t.Fatalf("status=%d calls=%d", w.Code, calls)
		}
	}
}

func TestRacerUnavailableFailsClosed(t *testing.T) {
	for _, missing := range []string{"backend", "client"} {
		t.Run(missing, func(t *testing.T) {
			var logs bytes.Buffer

			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			cfg := testConfig()
			cfg.RacerEnabled = true

			var backend mirror.ContentBackend
			if missing == "client" {
				backend = NewHandler(nil, nil, logger)
			}

			server := mirror.New(cfg, nil, &challengeOrigin{}, mirror.WithContentBackend(backend), mirror.WithLogger(logger))
			w := httptest.NewRecorder()
			server.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v2/library/image/blobs/"+digestOf(nil).String(), nil))

			if w.Code != http.StatusServiceUnavailable || w.Body.String() != "Racer unavailable\n" || logs.Len() == 0 {
				t.Fatalf("did not fail closed: %d %q", w.Code, w.Body.String())
			}

			if missing == "client" && w.Header().Get("Gantry-Racer-Request-ID") == "" {
				t.Fatal("missing correlation")
			}
		})
	}
}

func pageOrigin(data []byte) racersdk.Origin {
	metadata := racersdk.Metadata{Size: int64(len(data)), ETag: `"` + digestOf(data).String() + `"`, ContentType: "application/octet-stream", ExpiresAt: time.Now().Add(time.Hour)}

	return func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		if req.Head || req.Offset >= metadata.Size {
			return metadata, nil, nil
		}

		return metadata, io.NopCloser(bytes.NewReader(data[req.Offset:min(req.Offset+req.Length, metadata.Size)])), nil
	}
}

func handlerServer(t *testing.T, client Client) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(WrapHTTP(mirror.New(testConfig(), nil, &challengeOrigin{}, mirror.WithContentBackend(NewHandler(client, nil, nil))).Handler(), 0, nil))
	server.Client().Timeout = 10 * time.Second
	t.Cleanup(server.Close)

	return server
}

func TestRacerGETHEADAndResume(t *testing.T) {
	for _, size := range []int{0, 17, 2*int(racersdk.PageSize) + 37} {
		data := bytes.Repeat([]byte("x"), size)
		for _, mode := range []string{"GET", "HEAD", "resume", "invalid", "unsatisfiable"} {
			t.Run(fmt.Sprintf("%d/%s", size, mode), func(t *testing.T) {
				client := racersdktest.NewClient(t, pageOrigin(data))
				server := handlerServer(t, client)
				method, rangeHeader, status, offset := http.MethodGet, "", http.StatusOK, 0
				want := data

				switch mode {
				case "HEAD":
					method, want = http.MethodHead, nil
				case "resume":
					if size == 0 {
						return
					}

					offset, status = size-1, http.StatusPartialContent
					want, rangeHeader = data[offset:], fmt.Sprintf("bytes=%d-", offset)
				case "invalid":
					rangeHeader = "bytes=2-4"
				case "unsatisfiable":
					rangeHeader, status = fmt.Sprintf("bytes=%d-", size+1), http.StatusRequestedRangeNotSatisfiable
				}

				req, err := http.NewRequestWithContext(t.Context(), method, server.URL+"/v2/library/image/blobs/"+digestOf(data).String(), nil)
				if err != nil {
					t.Fatal(err)
				}

				req.Header.Set("Range", rangeHeader)

				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}

				got, err := io.ReadAll(resp.Body)
				resp.Body.Close()

				if err != nil || resp.StatusCode != status {
					t.Fatalf("status=%d err=%v", resp.StatusCode, err)
				}

				if mode == "unsatisfiable" {
					if resp.Header.Get("Content-Range") != fmt.Sprintf("bytes */%d", size) {
						t.Fatal("missing range size")
					}

					return
				}

				if !bytes.Equal(got, want) || resp.Header.Get("Docker-Content-Digest") != digestOf(data).String() || resp.Header.Get("Gantry-Mirrored") != "1" {
					t.Fatalf("bytes=%d headers=%v", len(got), resp.Header)
				}

				if offset > 0 && resp.Header.Get("Content-Range") != fmt.Sprintf("bytes %d-%d/%d", offset, size-1, size) {
					t.Fatal("incorrect resumed range")
				}
			})
		}
	}
}

func TestRacerErrorsAndCredentialPropagation(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{
		{racersdk.ErrNotFound, 404},
		{racersdk.ErrUnauthorized, 401},
		{racersdk.ErrForbidden, 403},
		{racersdk.ErrUnavailable, 503},
		{errors.New("private upstream detail"), 502},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var calls atomic.Int32

			client := racersdktest.NewClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				calls.Add(1)

				if req.Authorization != "Bearer delegated" || strings.Contains(req.Metadata, "delegated") {
					t.Error("credentials missing or leaked")
				}

				return racersdk.Metadata{}, nil, tc.err
			})
			server := handlerServer(t, client)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/library/image/blobs/"+digestOf(nil).String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Authorization", "Bearer delegated")

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()

			if err != nil || resp.StatusCode != tc.status || calls.Load() != 1 || strings.Contains(string(body), "private") {
				t.Fatalf("status=%d calls=%d err=%v", resp.StatusCode, calls.Load(), err)
			}
		})
	}
}

type repositoryChallenger struct{ calls int }

func (*repositoryChallenger) AuthenticationChallenge(context.Context, string) (string, bool, error) {
	panic("repository failure used root challenge")
}

func (c *repositoryChallenger) RepositoryAuthenticationChallenge(ctx context.Context, ref ifaces.OriginRef) (string, bool, error) {
	c.calls++

	if _, ok := ctx.Deadline(); !ok || ref != testRef() || registryauth.Authorization(ctx) != "" {
		return "", false, errors.New("invalid probe")
	}

	return `Basic realm="registry"`, true, nil
}

func TestRacerRepositoryChallengeOnlyOnUnauthorized(t *testing.T) {
	for _, err := range []error{racersdk.ErrUnauthorized, racersdk.ErrForbidden, racersdk.ErrNotFound, racersdk.ErrUnavailable} {
		probe := &repositoryChallenger{}
		w := httptest.NewRecorder()
		NewHandler(nil, probe, nil).racerError(w, httptest.NewRequest(http.MethodGet, "/", nil), testRef(), err)

		if errors.Is(err, racersdk.ErrUnauthorized) {
			if probe.calls != 1 || w.Header().Get("WWW-Authenticate") != `Basic realm="registry"` {
				t.Fatal("missing repository challenge")
			}
		} else if probe.calls != 0 || w.Header().Get("WWW-Authenticate") != "" {
			t.Fatal("non-401 probed registry")
		}
	}
}

func TestRacerTruncatedStreamAborts(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1<<20)

	for _, offset := range []int{0, 65536} {
		t.Run(fmt.Sprint(offset), func(t *testing.T) {
			client := racersdktest.NewClient(t, func(_ context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
				metadata, _, _ := pageOrigin(data)(t.Context(), racersdk.OriginRequest{Head: true})
				if req.Head {
					return metadata, nil, nil
				}

				return metadata, io.NopCloser(bytes.NewReader(data[:len(data)/2])), nil
			})
			server := handlerServer(t, client)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/library/image/blobs/"+digestOf(data).String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			if offset != 0 {
				req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
			}

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}

			got, err := io.ReadAll(resp.Body)
			resp.Body.Close()

			if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) >= len(data)-offset || !bytes.Equal(got, data[offset:offset+len(got)]) {
				t.Fatalf("truncation completed: %d %v", len(got), err)
			}
		})
	}
}

func TestRacerManifestSizeLimit(t *testing.T) {
	data := bytes.Repeat([]byte("x"), int(racersdk.PageSize)+1)
	client := racersdktest.NewClient(t, pageOrigin(data))
	server := handlerServer(t, client)

	resp, err := server.Client().Get(server.URL + "/v2/library/image/manifests/" + digestOf(data).String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" {
		t.Fatal("oversize manifest accepted")
	}
}

func TestRacerFailureDiagnostics(t *testing.T) {
	var logs bytes.Buffer

	handler := NewHandler(nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	ids := map[string]bool{}

	for i := range racerFailureLogBurst + 2 {
		logs.Reset()

		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("Gantry-Racer-Request-ID", "caller-secret")
		handler.ServeContent(w, r, testRef())

		id := w.Header().Get("Gantry-Racer-Request-ID")
		if len(id) < 26 || ids[id] || id == "caller-secret" {
			t.Fatal("invalid correlation")
		}

		ids[id] = true

		if (logs.Len() > 0) != (i < racerFailureLogBurst) {
			t.Fatal("unbounded diagnostic burst")
		}
	}

	logs.Reset()
	handler = NewHandler(nil, nil, slog.New(slog.NewJSONHandler(&logs, nil)))
	handler.logRacerFailure("generated", "get", fmt.Errorf("secret URL: %w", racersdk.ErrUnauthorized), -1, 0)

	if strings.Contains(logs.String(), "secret") || !strings.Contains(logs.String(), "unauthorized") {
		t.Fatal("unsafe diagnostic")
	}
}
