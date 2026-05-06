// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package daemon

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/unbounded/pkg/agent/goalstates"
)

const (
	agentUpgradeDownloadURLParameter = "downloadURL"
	agentBinaryArchiveName           = "unbounded-agent"
)

func agentUpgradeDownloadURL(parameters map[string]string) (string, error) {
	downloadURL := strings.TrimSpace(parameters[agentUpgradeDownloadURLParameter])
	if downloadURL == "" {
		return "", fmt.Errorf("missing required parameter %q", agentUpgradeDownloadURLParameter)
	}

	return downloadURL, nil
}

func upgradeDaemonBinary(ctx context.Context, log *slog.Logger, downloadURL string) error {
	currentTarget, err := resolveSymlink(daemonBinaryCurrentPath())
	if err != nil {
		return fmt.Errorf("resolve current daemon binary symlink: %w", err)
	}

	inactivePath := daemonBinaryBluePath()
	if currentTarget == daemonBinaryBluePath() {
		inactivePath = daemonBinaryGreenPath()
	}

	tmpDir, err := os.MkdirTemp("", "unbounded-agent-upgrade-*")
	if err != nil {
		return fmt.Errorf("create temp dir for agent upgrade: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	extractedBinaryPath := filepath.Join(tmpDir, agentBinaryArchiveName)
	if err := downloadAgentBinaryFromTarGz(ctx, downloadURL, extractedBinaryPath); err != nil {
		return err
	}

	binaryData, err := os.ReadFile(extractedBinaryPath)
	if err != nil {
		return fmt.Errorf("read extracted agent binary: %w", err)
	}

	if err := writeFile(inactivePath, binaryData, 0o755); err != nil {
		return fmt.Errorf("install upgraded daemon binary to %s: %w", inactivePath, err)
	}

	if err := updateSymlink(daemonBinaryLastGoodPath(), currentTarget); err != nil {
		return fmt.Errorf("update last-good daemon symlink: %w", err)
	}

	if err := updateSymlink(daemonBinaryCurrentPath(), inactivePath); err != nil {
		return fmt.Errorf("update current daemon symlink: %w", err)
	}

	log.Info("staged upgraded daemon binary",
		"url", downloadURL,
		"previous", currentTarget,
		"current", inactivePath,
	)

	return nil
}

func downloadAgentBinaryFromTarGz(ctx context.Context, downloadURL, targetPath string) error {
	reader, err := openDownloadStream(ctx, downloadURL)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	gzipReader, err := gzip.NewReader(reader)
	if err != nil {
		return fmt.Errorf("open gzip stream from %q: %w", downloadURL, err)
	}
	defer func() { _ = gzipReader.Close() }()

	tarReader := tar.NewReader(gzipReader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar archive from %q: %w", downloadURL, err)
		}

		if header.Typeflag != tar.TypeReg {
			continue
		}
		if filepath.Base(header.Name) != agentBinaryArchiveName {
			continue
		}

		file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			return fmt.Errorf("create extracted agent binary %s: %w", targetPath, err)
		}

		if _, copyErr := io.Copy(file, tarReader); copyErr != nil {
			_ = file.Close()
			return fmt.Errorf("extract agent binary from %q: %w", downloadURL, copyErr)
		}

		if closeErr := file.Close(); closeErr != nil {
			return fmt.Errorf("close extracted agent binary %s: %w", targetPath, closeErr)
		}

		return nil
	}

	return fmt.Errorf("agent binary %q not found in archive %q", agentBinaryArchiveName, downloadURL)
}

func openDownloadStream(ctx context.Context, downloadURL string) (io.ReadCloser, error) {
	parsedURL, err := url.Parse(downloadURL)
	if err != nil {
		return nil, fmt.Errorf("parse download URL %q: %w", downloadURL, err)
	}

	switch parsedURL.Scheme {
	case "http", "https":
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
		if err != nil {
			return nil, fmt.Errorf("create download request for %q: %w", downloadURL, err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("download agent archive from %q: %w", downloadURL, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("download agent archive from %q returned status %s", downloadURL, resp.Status)
		}

		return resp.Body, nil
	case "file":
		return os.Open(parsedURL.Path)
	default:
		return nil, fmt.Errorf("unsupported agent download URL scheme %q", parsedURL.Scheme)
	}
}

func resolveSymlink(path string) (string, error) {
	targetPath, err := filepath.EvalSymlinks(path)
	if err == nil {
		return targetPath, nil
	}

	if os.IsNotExist(err) {
		return daemonBinaryPath(), nil
	}

	return "", err
}

func daemonBinaryPath() string {
	if path := strings.TrimSpace(os.Getenv("UNBOUNDED_AGENT_DAEMON_BINARY")); path != "" {
		return path
	}

	return goalstates.DaemonBinaryPath
}

func daemonBinaryBluePath() string {
	if path := strings.TrimSpace(os.Getenv("UNBOUNDED_AGENT_DAEMON_BINARY_BLUE")); path != "" {
		return path
	}

	return goalstates.DaemonBinaryBluePath
}

func daemonBinaryGreenPath() string {
	if path := strings.TrimSpace(os.Getenv("UNBOUNDED_AGENT_DAEMON_BINARY_GREEN")); path != "" {
		return path
	}

	return goalstates.DaemonBinaryGreenPath
}

func daemonBinaryCurrentPath() string {
	if path := strings.TrimSpace(os.Getenv("UNBOUNDED_AGENT_DAEMON_BINARY_CURRENT")); path != "" {
		return path
	}

	return goalstates.DaemonBinaryCurrentPath
}

func daemonBinaryLastGoodPath() string {
	if path := strings.TrimSpace(os.Getenv("UNBOUNDED_AGENT_DAEMON_BINARY_LAST_GOOD")); path != "" {
		return path
	}

	return goalstates.DaemonBinaryLastGoodPath
}

func updateSymlink(linkPath, targetPath string) error {
	tmpPath := fmt.Sprintf("%s.tmp", linkPath)
	_ = os.Remove(tmpPath)
	if err := os.Symlink(targetPath, tmpPath); err != nil {
		return err
	}

	return os.Rename(tmpPath, linkPath)
}
