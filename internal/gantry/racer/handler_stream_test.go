// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/unbounded/internal/gantry/ifaces"
	"github.com/Azure/unbounded/pkg/racersdk"
	"github.com/Azure/unbounded/pkg/racersdk/racersdktest"
)

type metadataClient struct {
	*racersdk.Client
	metadata racersdk.Metadata
}

func (c metadataClient) Stat(context.Context, racersdk.Request) (racersdk.Metadata, error) {
	return c.metadata, nil
}

func TestRacerResumeRejectsMixedMetadata(t *testing.T) {
	data := []byte("0123456789")

	for _, change := range []string{"size", "version", "type", "absent type", "negative size"} {
		t.Run(change, func(t *testing.T) {
			metadata, _, _ := pageOrigin(data)(t.Context(), racersdk.OriginRequest{Head: true})

			switch change {
			case "size":
				metadata.Size++
			case "version":
				metadata.ETag = `"wrong"`
			case "type":
				metadata.ContentType = "application/custom"
			case "absent type":
				metadata.ContentType = ""
			case "negative size":
				metadata.Size = -1
			}

			client := metadataClient{Client: racersdktest.NewClient(t, pageOrigin(data)), metadata: metadata}
			server := handlerServer(t, client)

			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/library/image/blobs/"+digestOf(data).String(), nil)
			if err != nil {
				t.Fatal(err)
			}

			req.Header.Set("Range", "bytes=4-")

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusBadGateway || resp.Header.Get("Gantry-Mirrored") != "" {
				t.Fatal("mixed snapshot accepted")
			}
		})
	}
}

type transcriptClient struct {
	*racersdk.Client
	stats, gets atomic.Int32
	options     []racersdk.ReadOptions
}

func (c *transcriptClient) Stat(ctx context.Context, req racersdk.Request) (racersdk.Metadata, error) {
	c.stats.Add(1)
	return c.Client.Stat(ctx, req)
}

func (c *transcriptClient) Get(ctx context.Context, req racersdk.Request, opts ...racersdk.ReadOptions) (*racersdk.Object, error) {
	c.gets.Add(1)
	c.options = opts

	return c.Client.Get(ctx, req, opts...)
}

func TestRacerSDKRequestTranscript(t *testing.T) {
	for _, mode := range []string{"GET", "HEAD", "manifest", "resume", "unsatisfiable"} {
		t.Run(mode, func(t *testing.T) {
			data := []byte("0123456789")
			client := &transcriptClient{Client: racersdktest.NewClient(t, pageOrigin(data))}
			ref := testRef()
			ref.Digest = digestOf(data)
			method := http.MethodGet
			stats, gets := int32(0), int32(1)

			switch mode {
			case "HEAD":
				method, stats, gets = http.MethodHead, 1, 0
			case "manifest":
				ref.Kind = ifaces.KindManifest
			case "resume":
				ref.Offset, stats = 4, 1
			case "unsatisfiable":
				ref.Offset, stats, gets = 10, 1, 0
			}

			w := httptest.NewRecorder()
			NewHandler(client, nil, nil).ServeContent(w, httptest.NewRequest(method, "/", nil), ref)

			if client.stats.Load() != stats || client.gets.Load() != gets {
				t.Fatalf("stats=%d gets=%d", client.stats.Load(), client.gets.Load())
			}

			if mode == "manifest" && (len(client.options) != 1 || !client.options[0].SmallObject) {
				t.Fatal("manifest used bulk capacity")
			}

			if mode == "GET" && len(client.options) != 0 {
				t.Fatal("GET added redundant options")
			}

			if mode == "resume" && (len(client.options) != 1 || client.options[0].Offset != 4 || client.options[0].ETag != `"`+ref.Digest.String()+`"`) {
				t.Fatal("resume not pinned")
			}
		})
	}
}

func distinctPayload(object uint64, size int) []byte {
	data := make([]byte, size)

	var seed [16]byte
	binary.LittleEndian.PutUint64(seed[:8], object)

	for offset := 0; offset < size; offset += sha256.Size {
		binary.LittleEndian.PutUint64(seed[8:], uint64(offset))
		block := sha256.Sum256(seed[:])
		copy(data[offset:], block[:])
	}

	return data
}

func TestRacerConcurrentDistinctStreams(t *testing.T) {
	objects := [][]byte{distinctPayload(1, int(racersdk.PageSize)+65539), distinctPayload(2, int(racersdk.PageSize)+65539)}
	origins := map[racersdk.Key]racersdk.Origin{}

	for _, data := range objects {
		ref := testRef()
		ref.Digest = digestOf(data)
		origins[requestFor(t, ref, "").Key] = pageOrigin(data)
	}

	client := racersdktest.NewClient(t, func(ctx context.Context, req racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return origins[req.Key](ctx, req)
	})
	server := handlerServer(t, client)

	var wg sync.WaitGroup

	for _, data := range objects {
		for _, offset := range []int{0, int(racersdk.PageSize) - 7, int(racersdk.PageSize), len(data) - 1} {
			wg.Go(func() {
				req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/v2/library/image/blobs/"+digestOf(data).String(), nil)
				if err != nil {
					t.Error(err)
					return
				}

				if offset != 0 {
					req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
				}

				resp, err := server.Client().Do(req)
				if err != nil {
					t.Error(err)
					return
				}
				defer resp.Body.Close()

				hash := sha256.New()
				n, err := io.Copy(hash, resp.Body)

				want := sha256.Sum256(data[offset:])
				if err != nil || n != int64(len(data)-offset) || !bytes.Equal(hash.Sum(nil), want[:]) {
					t.Errorf("offset=%d bytes=%d err=%v", offset, n, err)
				}
			})
		}
	}

	wg.Wait()
}

type blockingBody struct {
	entered, closed     chan struct{}
	readOnce, closeOnce sync.Once
}

func (b *blockingBody) Read([]byte) (int, error) {
	b.readOnce.Do(func() { close(b.entered) })
	<-b.closed

	return 0, io.ErrClosedPipe
}

func (b *blockingBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestRacerCancellationClosesOriginBody(t *testing.T) {
	body := &blockingBody{entered: make(chan struct{}), closed: make(chan struct{})}
	client := racersdktest.NewClient(t, func(context.Context, racersdk.OriginRequest) (racersdk.Metadata, io.ReadCloser, error) {
		return racersdk.Metadata{Size: 100, ETag: `"` + testRef().Digest.String() + `"`, ExpiresAt: time.Now().Add(time.Hour)}, body, nil
	})
	server := handlerServer(t, client)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v2/library/image/blobs/"+testRef().Digest.String(), nil)
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)

	go func() {
		resp, err := server.Client().Do(req)
		if err == nil {
			_, err = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		done <- err
	}()

	select {
	case <-body.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("body did not start")
	}

	cancel()

	select {
	case <-body.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not close body")
	}

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not stop")
	}
}
