//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// verifyConsumerCorruption uses the running kind containerd, including its HTTP
// retry reader and content-store commit verification. Corruption is injected
// after Gantry to test the consumer trust boundary without poisoning Racer.
func (h *harness) verifyConsumerCorruption(worker, gateway string, mirrorURL *url.URL, fixtures [2]*image) {
	h.t.Helper()
	// Fresh digests avoid containerd's shared physical content store, whose
	// garbage collection can outlive deletion of a namespace's content record.
	for index, mode := range []string{"full", "resumed assembly"} {
		h.t.Run("consumer rejects corruption/"+mode, func(t *testing.T) {
			fixture := fixtures[index]

			var layer string

			for id, blob := range fixture.blobs {
				if blob.mediaType == "application/vnd.oci.image.layer.v1.tar+gzip" {
					layer = id
				}
			}

			require.NotEmpty(t, layer)

			contentNamespace := "racer-corrupt-" + strings.ReplaceAll(mode, " ", "-")

			var (
				full, resumed atomic.Int32
				resumedOffset atomic.Int64
			)

			proxy := httputil.NewSingleHostReverseProxy(mirrorURL)
			// Flush the unaligned prefix before the injected read error aborts
			// the handler, so the resume offset proves exact retained assembly.
			proxy.FlushInterval = -1
			proxy.ModifyResponse = func(response *http.Response) error {
				if response.Request.Method != http.MethodGet || !strings.HasSuffix(response.Request.URL.Path, "/"+layer) {
					return nil
				}

				if response.Header.Get("Gantry-Mirrored") != "1" {
					return fmt.Errorf("corruption fixture bypassed Gantry")
				}

				var offset int64
				if value := response.Request.Header.Get("Range"); value != "" {
					if _, err := fmt.Sscanf(value, "bytes=%d-", &offset); err != nil {
						return err
					}
				}
				// Containerd may start a full fetch with Range: bytes=0-.
				if offset > 0 {
					resumed.Add(1)

					if response.StatusCode != http.StatusPartialContent {
						return fmt.Errorf("resume status %d", response.StatusCode)
					}

					resumedOffset.Store(offset)

					return nil
				}

				if response.StatusCode != http.StatusOK {
					return fmt.Errorf("full read status %d", response.StatusCode)
				}

				full.Add(1)

				body, err := io.ReadAll(response.Body)
				response.Body.Close()

				if err != nil {
					return err
				}

				if len(body) != len(fixture.blobs[layer].body) {
					return fmt.Errorf("incomplete layer before corruption injection: %d", len(body))
				}

				body[0] ^= 0xff
				if mode == "resumed assembly" {
					// Preserve the original Content-Length while interrupting
					// after a corrupt prefix beyond page zero. Containerd must
					// retain that prefix and request the missing suffix.
					body = body[:(16<<20)+7]
					response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), corruptionEOF{}))
				} else {
					response.Body = io.NopCloser(bytes.NewReader(body))
				}

				return nil
			}
			port := h.serve(proxy)
			// Each fault phase has its own endpoint and resolver configuration.
			hostsDir := "/etc/containerd/" + contentNamespace
			h.run("docker", "exec", worker, "mkdir", "-p", hostsDir+"/"+registry)
			h.write("corrupt-hosts.toml", fmt.Sprintf("server = %q\n", "http://"+net.JoinHostPort(gateway, port)))
			h.run("docker", "cp", filepath.Join(h.artifacts, "corrupt-hosts.toml"), worker+":"+hostsDir+"/"+registry+"/hosts.toml")

			ctx, cancel := context.WithTimeout(h.ctx, time.Minute)
			defer cancel()

			cmd := exec.CommandContext(ctx, "docker", "exec", worker, "ctr", "-n", contentNamespace, "images", "pull", "--local", "--hosts-dir", hostsDir, registry+"/fixture/image@"+fixture.manifest)
			output, err := cmd.CombinedOutput()
			h.write(contentNamespace+".log", string(output))
			t.Logf("faulty transport: full=%d resumed=%d offset=%d", full.Load(), resumed.Load(), resumedOffset.Load())
			require.Error(t, err, "containerd accepted corrupt OCI bytes: %s", output)
			require.Contains(t, string(output), "digest", "failure must be digest verification: %s", output)
			require.Contains(t, string(output), "failed precondition", "failure must be content commit: %s", output)
			require.Positive(t, full.Load())

			if mode == "resumed assembly" {
				require.Positive(t, resumed.Load(), "containerd never requested a resume")
				require.Equal(t, int64((16<<20)+7), resumedOffset.Load(), "containerd must retain the corrupt prefix and fetch only the suffix")
			}

			stored := h.run("docker", "exec", worker, "ctr", "-n", contentNamespace, "content", "ls", "-q")
			require.NotContains(t, stored, layer, "corrupt object was committed")
		})
	}
}

type corruptionEOF struct{}

func (corruptionEOF) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
