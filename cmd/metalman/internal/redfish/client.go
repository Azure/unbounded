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
	httpClient := newHttpClient(certSHA256)
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
		s.delete()
		return nil, err
	}

	return &Client{session: s, deviceID: id}, nil
}

// Close releases the client's Redfish session.
func (c *Client) Close() {
	c.session.delete()
}

// PowerState returns the current power state (e.g. "On", "Off").
func (c *Client) PowerState(ctx context.Context) (string, error) {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	data, status, err := c.session.get(ctx, path)
	if err != nil {
		return "", err
	}

	if status != http.StatusOK {
		return "", fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var result struct {
		PowerState string `json:"PowerState"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing power state: %w", err)
	}

	return result.PowerState, nil
}

// Reset sends a ComputerSystem.Reset action (e.g. "ForceOff", "On").
func (c *Client) Reset(ctx context.Context, resetType string) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s/Actions/ComputerSystem.Reset", c.deviceID)
	body := map[string]string{"ResetType": resetType}

	data, status, err := c.session.post(ctx, path, body)
	if err != nil {
		return err
	}

	if status != http.StatusOK && status != http.StatusNoContent && status != http.StatusAccepted {
		return fmt.Errorf("unexpected status %d from reset %s: %s", status, resetType, data)
	}

	return nil
}

// BootOrderConfig sets or clears the boot source override.
// Returns ErrUnsupported if the BMC does not support boot order PATCH.
func (c *Client) BootOrderConfig(ctx context.Context, log *slog.Logger, pendingReimage bool) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	data, status, err := c.session.get(ctx, path)
	if err != nil {
		return err
	}

	if status != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s: %s", status, path, data)
	}

	var system struct {
		Boot struct {
			BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return fmt.Errorf("parsing system boot config: %w", err)
	}

	var body map[string]any

	if pendingReimage {
		if system.Boot.BootSourceOverrideTarget == "Pxe" && system.Boot.BootSourceOverrideEnabled == "Continuous" {
			return nil
		}

		body = map[string]any{
			"Boot": map[string]string{
				"BootSourceOverrideTarget":  "Pxe",
				"BootSourceOverrideEnabled": "Continuous",
			},
		}

		log.Info("setting boot source override to PXE",
			"currentTarget", system.Boot.BootSourceOverrideTarget,
			"currentEnabled", system.Boot.BootSourceOverrideEnabled)
	} else {
		if system.Boot.BootSourceOverrideEnabled == "Disabled" ||
			(system.Boot.BootSourceOverrideTarget == "Hdd" && system.Boot.BootSourceOverrideEnabled == "Continuous") {
			return nil
		}

		body = map[string]any{
			"Boot": map[string]string{
				"BootSourceOverrideEnabled": "Disabled",
			},
		}

		log.Info("disabling boot source override",
			"currentTarget", system.Boot.BootSourceOverrideTarget,
			"currentEnabled", system.Boot.BootSourceOverrideEnabled)
	}

	_, patchStatus, err := c.session.patch(ctx, path, body)
	if err != nil {
		return err
	}

	if statusUnsupported(patchStatus) {
		if !pendingReimage {
			// Some BMCs do not support disabling the boot source override.
			// Fall back to setting it to Hdd, which prevents PXE boot.
			log.Info("falling back to Hdd boot source override",
				"currentTarget", system.Boot.BootSourceOverrideTarget,
				"currentEnabled", system.Boot.BootSourceOverrideEnabled)

			body = map[string]any{
				"Boot": map[string]string{
					"BootSourceOverrideTarget":  "Hdd",
					"BootSourceOverrideEnabled": "Continuous",
				},
			}

			_, patchStatus, err = c.session.patch(ctx, path, body)
			if err != nil {
				return err
			}

			if statusUnsupported(patchStatus) {
				return fmt.Errorf("boot order config PATCH returned %d: %w", patchStatus, ErrUnsupported)
			}

			if patchStatus != http.StatusOK && patchStatus != http.StatusNoContent && patchStatus != http.StatusAccepted {
				return fmt.Errorf("unexpected status %d from boot order PATCH", patchStatus)
			}

			return nil
		}

		return fmt.Errorf("boot order config PATCH returned %d: %w", patchStatus, ErrUnsupported)
	}

	if patchStatus != http.StatusOK && patchStatus != http.StatusNoContent && patchStatus != http.StatusAccepted {
		return fmt.Errorf("unexpected status %d from boot order PATCH", patchStatus)
	}

	return nil
}

// CaptureFingerprint connects to a BMC without cert pinning and returns
// the SHA256 fingerprint of its TLS certificate (for TOFU).
func CaptureFingerprint(ctx context.Context, url string) (string, error) {
	httpClient := newHttpClient("")
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

// newHttpClient returns an *http.Client with TLS cert pinning.
// If certSHA256 is empty (TOFU), any certificate is accepted.
func newHttpClient(certSHA256 string) *http.Client {
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

// resolveDeviceID returns the given deviceID if non-empty, or discovers it by
// querying /redfish/v1/Systems and returning the first (typically only) member.
func resolveDeviceID(ctx context.Context, s *bmcSession, deviceID string) (string, error) {
	if deviceID != "" {
		return deviceID, nil
	}

	data, status, err := s.get(ctx, "/redfish/v1/Systems")
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

// statusUnsupported returns true for HTTP status codes that indicate the BMC
// permanently does not support the requested resource or operation. Per the
// Redfish specification (DSP0266 §8.3):
//   - 400: request body contains unsupported property values
//   - 404: resource does not exist
//   - 405: resource exists but does not support the HTTP method
//   - 410: resource has been permanently removed
//   - 501: service does not implement the HTTP method at all
func statusUnsupported(code int) bool {
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
