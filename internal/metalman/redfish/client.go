// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package redfish

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// ErrUnsupported indicates the BMC does not support the requested operation.
var ErrUnsupported = errors.New("not supported by BMC")

// PowerState represents the power state of a Redfish system.
type PowerState string

const (
	PowerOn  PowerState = "On"
	PowerOff PowerState = "Off"
)

// ResetType represents a Redfish ComputerSystem.Reset action type.
type ResetType string

const (
	ResetForceOff ResetType = "ForceOff"
	ResetOn       ResetType = "On"
)

// BootTarget represents a Redfish boot source override target.
type BootTarget string

const (
	BootTargetPxe BootTarget = "Pxe"
	BootTargetHdd BootTarget = "Hdd"
)

// BootEnabled represents a Redfish boot source override enabled mode.
type BootEnabled string

const (
	BootContinuous BootEnabled = "Continuous"
	BootDisabled   BootEnabled = "Disabled"
)

// BootConfig holds the current boot source override configuration.
type BootConfig struct {
	Target  BootTarget
	Enabled BootEnabled
}

// Client provides Redfish operations against a single BMC.
// Created via Pool.Get or Dial. Must be closed when no longer needed.
type Client struct {
	session  *bmcSession
	deviceID string
}

// Dial connects to a BMC and returns a ready-to-use Client.
// It creates a Redfish session (falling back to basic auth) and
// resolves the device ID. The caller must call Close when done.
func Dial(ctx context.Context, url, certSHA256, user, pass, deviceID string) (*Client, error) {
	httpClient := newHTTPClient(certSHA256)
	s := &bmcSession{
		httpClient: httpClient,
		baseURL:    url,
		user:       user,
		pass:       pass,
	}

	token, location, err := createSession(ctx, httpClient, url, user, pass)
	if err != nil {
		slog.Info("Redfish session not available, using basic auth", "url", url, "err", err)
	} else {
		s.token = token
		s.location = location
	}

	id, err := resolveDeviceID(ctx, s, deviceID)
	if err != nil {
		s.close()
		return nil, err
	}

	return &Client{session: s, deviceID: id}, nil
}

// Close releases the client's Redfish session.
func (c *Client) Close() {
	c.session.close()
}

// PowerState returns the current power state of the system.
func (c *Client) PowerState(ctx context.Context) (PowerState, error) {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	data, status, err := c.session.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var result struct {
		PowerState PowerState `json:"PowerState"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing power state: %w", err)
	}

	return result.PowerState, nil
}

// Reset sends a ComputerSystem.Reset action.
func (c *Client) Reset(ctx context.Context, resetType ResetType) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s/Actions/ComputerSystem.Reset", c.deviceID)
	body := map[string]ResetType{"ResetType": resetType}

	data, status, err := c.session.do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}

	if !isSuccessStatus(status) {
		return fmt.Errorf("unexpected status %d from reset %s: %s", status, resetType, data)
	}

	return nil
}

// GetBootConfig returns the current boot source override configuration.
func (c *Client) GetBootConfig(ctx context.Context) (BootConfig, error) {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	data, status, err := c.session.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return BootConfig{}, err
	}

	if status != http.StatusOK {
		return BootConfig{}, fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var system struct {
		Boot struct {
			BootSourceOverrideTarget  BootTarget  `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled BootEnabled `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return BootConfig{}, fmt.Errorf("parsing system boot config: %w", err)
	}

	return BootConfig{
		Target:  system.Boot.BootSourceOverrideTarget,
		Enabled: system.Boot.BootSourceOverrideEnabled,
	}, nil
}

// SetBootOverride sets the boot source override target and enabled mode.
// Returns ErrUnsupported if the BMC does not support the PATCH.
func (c *Client) SetBootOverride(ctx context.Context, target BootTarget, enabled BootEnabled) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	body := map[string]any{
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  string(target),
			"BootSourceOverrideEnabled": string(enabled),
		},
	}

	_, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return fmt.Errorf("boot override PATCH returned %d: %w", status, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return fmt.Errorf("unexpected status %d from boot override PATCH", status)
	}

	return nil
}

// DisableBootOverride disables the boot source override. If the BMC does
// not support disabling, it falls back to setting Hdd/Continuous.
// Returns ErrUnsupported if neither approach works.
func (c *Client) DisableBootOverride(ctx context.Context) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	body := map[string]any{
		"Boot": map[string]string{
			"BootSourceOverrideEnabled": string(BootDisabled),
		},
	}

	_, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isSuccessStatus(status) {
		return nil
	}

	if !isUnsupportedStatus(status) {
		return fmt.Errorf("unexpected status %d from boot override PATCH", status)
	}

	// Some BMCs do not support disabling the boot source override.
	// Fall back to setting Hdd/Continuous, which prevents PXE boot.
	slog.Info("BMC does not support Disabled boot override, falling back to Hdd")

	return c.SetBootOverride(ctx, BootTargetHdd, BootContinuous)
}

// CaptureFingerprint connects to a BMC without cert pinning and returns
// the SHA256 fingerprint of its TLS certificate (for TOFU).
func CaptureFingerprint(ctx context.Context, url string) (string, error) {
	httpClient := newHTTPClient("")
	defer httpClient.CloseIdleConnections()

	endpoint := strings.TrimRight(url, "/") + "/redfish/v1/"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("connecting to Redfish endpoint: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	io.Copy(io.Discard, resp.Body) //nolint:errcheck // Best-effort drain of response body.

	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		return "", fmt.Errorf("no TLS peer certificates")
	}

	return formatFingerprint(sha256Sum(resp.TLS.PeerCertificates[0].Raw)), nil
}

// newHTTPClient returns an *http.Client with TLS cert pinning.
// If certSHA256 is empty (TOFU mode), any certificate is accepted.
func newHTTPClient(certSHA256 string) *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
				VerifyConnection: func(cs tls.ConnectionState) error {
					if certSHA256 == "" {
						return nil
					}

					if len(cs.PeerCertificates) == 0 {
						return fmt.Errorf("no TLS peer certificates")
					}

					fp := formatFingerprint(sha256Sum(cs.PeerCertificates[0].Raw))
					if fp != certSHA256 {
						return fmt.Errorf("TLS cert SHA256 mismatch: got %s, want %s", fp, certSHA256)
					}

					return nil
				},
			},
		},
	}
}

// resolveDeviceID returns the given deviceID if non-empty, or discovers it
// by querying /redfish/v1/Systems and returning the first member.
func resolveDeviceID(ctx context.Context, s *bmcSession, deviceID string) (string, error) {
	if deviceID != "" {
		return deviceID, nil
	}

	data, status, err := s.do(ctx, http.MethodGet, "/redfish/v1/Systems", nil)
	if err != nil {
		return "", err
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d from /redfish/v1/Systems: %s", status, data)
	}

	var collection struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(data, &collection); err != nil {
		return "", fmt.Errorf("parsing Systems collection: %w", err)
	}

	if len(collection.Members) == 0 {
		return "", fmt.Errorf("no members in /redfish/v1/Systems")
	}

	// Extract the system ID from the last path segment.
	id := collection.Members[0].ODataID
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
	}

	return id, nil
}

// isSuccessStatus returns true for HTTP status codes indicating success.
func isSuccessStatus(code int) bool {
	return code == http.StatusOK || code == http.StatusNoContent || code == http.StatusAccepted
}

// isUnsupportedStatus returns true for HTTP status codes that indicate the BMC
// permanently does not support the requested resource or operation. Per the
// Redfish specification (DSP0266 §8.3):
//   - 400: request body contains unsupported property values
//   - 404: resource does not exist
//   - 405: resource exists but does not support the HTTP method
//   - 410: resource has been permanently removed
//   - 501: service does not implement the HTTP method at all
func isUnsupportedStatus(code int) bool {
	return code == http.StatusBadRequest ||
		code == http.StatusNotFound ||
		code == http.StatusMethodNotAllowed ||
		code == http.StatusGone ||
		code == http.StatusNotImplemented
}

func formatFingerprint(b []byte) string {
	parts := make([]string, len(b))
	for i, v := range b {
		parts[i] = fmt.Sprintf("%02x", v)
	}

	return strings.Join(parts, ":")
}

func sha256Sum(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}
