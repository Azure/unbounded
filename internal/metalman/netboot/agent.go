// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	v1alpha3 "github.com/Azure/unbounded/api/machina/v1alpha3"
)

// DefaultAgentBaseURL is the release location the unbounded-agent tarball is
// downloaded from when neither the Machine nor the Site overrides it.
const DefaultAgentBaseURL = "https://github.com/Azure/unbounded/releases"

// AgentArtifact downloads the unbounded-agent tarball and serves it to hosts
// from the metalman HTTP server, so a netbooted host does not need to reach
// GitHub.
type AgentArtifact struct {
	// CacheDir is the metalman cache directory the tarballs are stored in.
	CacheDir string
	// DefaultBaseURL is the release base URL used when the Machine and the
	// Site do not set one.
	DefaultBaseURL string
	// DefaultVersion is the agent version downloaded when the Machine does
	// not pin one. An empty or non-release version falls back to the release
	// feed's latest.
	DefaultVersion string
	// Client fetches tarballs. Nil means a client with a download timeout.
	Client *http.Client

	mu       sync.Mutex
	keyLocks map[string]*sync.Mutex
}

// AgentSource is where one host's agent tarball comes from.
type AgentSource struct {
	// URL is the tarball location.
	URL string
	// ChecksumsURL is the release checksums file, empty when the source
	// cannot provide one.
	ChecksumsURL string
	// CacheKey names the cache entry.
	CacheKey string
	// FileName is the tarball name inside the cache entry.
	FileName string
}

// SourceFor resolves where the agent tarball for a Machine comes from.
func (a *AgentArtifact) SourceFor(machine *v1alpha3.Machine) AgentSource {
	architecture := v1alpha3.DefaultPXEArchitecture

	if machine != nil && machine.Spec.Netboot() != nil {
		architecture = machine.Spec.Netboot().TargetArchitecture()
	}

	fileName := fmt.Sprintf("unbounded-agent-linux-%s.tar.gz", architecture)
	agent := (*v1alpha3.AgentSpec)(nil)

	if machine != nil {
		agent = machine.Spec.Agent
	}

	if agent != nil && agent.URL != "" {
		return AgentSource{
			URL:      agent.URL,
			CacheKey: urlCacheKey(agent.URL),
			FileName: fileName,
		}
	}

	baseURL := strings.TrimRight(a.DefaultBaseURL, "/")
	version := a.DefaultVersion

	if agent != nil {
		if agent.BaseURL != "" {
			baseURL = strings.TrimRight(agent.BaseURL, "/")
		}

		if agent.Version != "" {
			version = agent.Version
		}
	}

	if version == "" || version == "dev" {
		return AgentSource{
			URL:      fmt.Sprintf("%s/latest/download/%s", baseURL, fileName),
			CacheKey: fmt.Sprintf("latest-%s", architecture),
			FileName: fileName,
		}
	}

	return AgentSource{
		URL:          fmt.Sprintf("%s/download/%s/%s", baseURL, version, fileName),
		ChecksumsURL: fmt.Sprintf("%s/download/%s/checksums.txt", baseURL, version),
		CacheKey:     fmt.Sprintf("%s-%s", version, architecture),
		FileName:     fileName,
	}
}

// Path returns the cached tarball for a Machine, downloading it on first use.
func (a *AgentArtifact) Path(ctx context.Context, machine *v1alpha3.Machine) (string, error) {
	source := a.SourceFor(machine)
	dest := filepath.Join(a.CacheDir, "agent", source.CacheKey, source.FileName)

	if fileExists(dest) {
		return dest, nil
	}

	unlock := a.lock(source.CacheKey)
	defer unlock()

	if fileExists(dest) {
		return dest, nil
	}

	return dest, a.download(ctx, source, dest)
}

func (a *AgentArtifact) lock(key string) func() {
	a.mu.Lock()

	if a.keyLocks == nil {
		a.keyLocks = make(map[string]*sync.Mutex)
	}

	lock, ok := a.keyLocks[key]
	if !ok {
		lock = &sync.Mutex{}
		a.keyLocks[key] = lock
	}

	a.mu.Unlock()

	lock.Lock()

	return lock.Unlock
}

func (a *AgentArtifact) download(ctx context.Context, source AgentSource, dest string) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("creating agent cache dir: %w", err)
	}

	sum, err := a.fetch(ctx, source.URL, dest+".tmp")
	if err != nil {
		os.Remove(dest + ".tmp") //nolint:errcheck // Best effort cleanup.

		return fmt.Errorf("downloading agent tarball: %w", err)
	}

	if err := a.verify(ctx, source, filepath.Base(dest), sum); err != nil {
		os.Remove(dest + ".tmp") //nolint:errcheck // Best effort cleanup.

		return err
	}

	if err := os.Rename(dest+".tmp", dest); err != nil {
		os.Remove(dest + ".tmp") //nolint:errcheck // Best effort cleanup.

		return fmt.Errorf("storing agent tarball: %w", err)
	}

	slog.InfoContext(ctx, "cached agent tarball", "url", source.URL, "path", dest, "sha256", sum)

	return nil
}

// fetch downloads a URL into dest and returns the SHA-256 of what it wrote.
func (a *AgentArtifact) fetch(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Read-only response body.

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", redactURL(url), resp.Status)
	}

	file, err := os.Create(dest)
	if err != nil {
		return "", err
	}

	hash := sha256.New()

	if _, err := io.Copy(io.MultiWriter(file, hash), resp.Body); err != nil {
		file.Close() //nolint:errcheck // Best effort cleanup.

		return "", err
	}

	if err := file.Close(); err != nil {
		return "", err
	}

	return hex.EncodeToString(hash.Sum(nil)), nil
}

// verify checks the download against the release checksums file. A missing or
// unreadable checksums file only skips verification: the tarball is still
// fetched from the same release.
func (a *AgentArtifact) verify(ctx context.Context, source AgentSource, fileName, sum string) error {
	if source.ChecksumsURL == "" {
		return nil
	}

	want, err := a.checksum(ctx, source.ChecksumsURL, fileName)
	if err != nil {
		slog.WarnContext(ctx, "agent checksums unavailable, skipping verification", "url", redactURL(source.ChecksumsURL), "err", err)

		return nil
	}

	if want != sum {
		return fmt.Errorf("agent tarball checksum mismatch: got %s, want %s", sum, want)
	}

	return nil
}

func (a *AgentArtifact) checksum(ctx context.Context, url, fileName string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}

	resp, err := a.client().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // Read-only response body.

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", redactURL(url), resp.Status)
	}

	scanner := bufio.NewScanner(resp.Body)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == fileName {
			return strings.ToLower(fields[0]), nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", errors.New("checksums file does not list the agent tarball")
}

func (a *AgentArtifact) client() *http.Client {
	if a.Client != nil {
		return a.Client
	}

	return &http.Client{Timeout: 10 * time.Minute}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && !info.IsDir()
}

// urlCacheKey names a cache entry after a URL that has no version.
func urlCacheKey(url string) string {
	sum := sha256.Sum256([]byte(url))

	return "url-" + hex.EncodeToString(sum[:8])
}

// redactURL drops the query string so signed URLs are not logged.
func redactURL(raw string) string {
	if index := strings.IndexByte(raw, '?'); index >= 0 {
		return raw[:index]
	}

	return raw
}
