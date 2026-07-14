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
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrUnsupported indicates the BMC does not support the requested operation.
var ErrUnsupported = errors.New("not supported by BMC")

type responseError struct {
	method string
	path   string
	status int
	body   []byte
	cause  error
}

func redfishResponseError(method, path string, status int, body []byte, cause error) error {
	return &responseError{
		method: method,
		path:   path,
		status: status,
		body:   append([]byte(nil), body...),
		cause:  cause,
	}
}

func (e *responseError) Error() string {
	if len(e.body) == 0 {
		return fmt.Sprintf("Redfish %s %s returned HTTP %d", e.method, e.path, e.status)
	}

	return fmt.Sprintf("Redfish %s %s returned HTTP %d: %s", e.method, e.path, e.status, e.body)
}

func (e *responseError) Unwrap() error {
	return e.cause
}

// PowerState represents the power state of a Redfish system.
type PowerState string

const (
	PowerOn  PowerState = "On"
	PowerOff PowerState = "Off"
)

// ResetType represents a Redfish ComputerSystem.Reset action type.
type ResetType string

const (
	ResetForceOff     ResetType = "ForceOff"
	ResetForceRestart ResetType = "ForceRestart"
	ResetOn           ResetType = "On"
)

// BootTarget represents a Redfish boot source override target.
type BootTarget string

const (
	BootTargetPxe      BootTarget = "Pxe"
	BootTargetHdd      BootTarget = "Hdd"
	BootTargetUefiHTTP BootTarget = "UefiHttp"
)

// BootEnabled represents a Redfish boot source override enabled mode.
type BootEnabled string

const (
	BootContinuous BootEnabled = "Continuous"
	BootOnce       BootEnabled = "Once"
	BootDisabled   BootEnabled = "Disabled"
)

// BootMode represents a Redfish boot source override mode.
type BootMode string

const (
	BootModeUEFI BootMode = "UEFI"
)

// BootConfig holds the current boot source override configuration.
type BootConfig struct {
	Target         BootTarget
	Enabled        BootEnabled
	Mode           BootMode
	UefiHTTPSource string
	HasHTTPBootURI bool
}

// StaticIPv4Config holds static host NIC settings for Redfish EthernetInterface.
type StaticIPv4Config struct {
	MAC        string
	Address    string
	SubnetMask string
	Gateway    string
	DNS        []string
}

const biosHTTPBootURIAttribute = "UrlBootFile"

const (
	biosDHCPv4Attribute         = "Dhcpv4"
	biosDHCPv4DisabledValue     = "Disabled"
	biosIPv4AddressAttribute    = "Ipv4Address"
	biosIPv4SubnetMaskAttribute = "Ipv4SubnetMask"
	biosIPv4GatewayAttribute    = "Ipv4Gateway"
	biosIPv4PrimaryDNSAttribute = "Ipv4PrimaryDNS"
)

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

	slog.Info("Redfish dialed BMC", "url", url, "device", id)

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
		return "", redfishResponseError(http.MethodGet, path, status, data, nil)
	}

	var result struct {
		PowerState PowerState `json:"PowerState"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing power state: %w", err)
	}

	slog.Info("Redfish read power state", "device", c.deviceID, "powerState", result.PowerState)

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
		return fmt.Errorf("reset %s failed: %w", resetType, redfishResponseError(http.MethodPost, path, status, data, nil))
	}

	slog.Info("Redfish reset", "device", c.deviceID, "resetType", resetType)

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
		return BootConfig{}, redfishResponseError(http.MethodGet, path, status, data, nil)
	}

	var system struct {
		Boot struct {
			BootSourceOverrideTarget  BootTarget      `json:"BootSourceOverrideTarget"`
			BootSourceOverrideEnabled BootEnabled     `json:"BootSourceOverrideEnabled"`
			BootSourceOverrideMode    BootMode        `json:"BootSourceOverrideMode"`
			HTTPBootURI               json.RawMessage `json:"HttpBootUri"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return BootConfig{}, fmt.Errorf("parsing system boot config: %w", err)
	}

	config := BootConfig{
		Target:         system.Boot.BootSourceOverrideTarget,
		Enabled:        system.Boot.BootSourceOverrideEnabled,
		Mode:           system.Boot.BootSourceOverrideMode,
		HasHTTPBootURI: system.Boot.HTTPBootURI != nil,
	}
	if system.Boot.HTTPBootURI != nil && string(system.Boot.HTTPBootURI) != "null" {
		if err := json.Unmarshal(system.Boot.HTTPBootURI, &config.UefiHTTPSource); err != nil {
			return BootConfig{}, fmt.Errorf("parsing system HTTP boot URI: %w", err)
		}
	}

	slog.Info("Redfish read boot config", "device", c.deviceID, "target", config.Target, "enabled", config.Enabled, "mode", config.Mode)

	return config, nil
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

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish set boot override", "device", c.deviceID, "target", target, "enabled", enabled)

	return nil
}

// SetHTTPBootOverride sets a persistent Redfish UEFI HTTP boot override.
// Returns ErrUnsupported if the BMC does not support the PATCH.
func (c *Client) SetHTTPBootOverride(ctx context.Context, bootURL string) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	body := map[string]any{
		"Boot": map[string]string{
			"BootSourceOverrideTarget":  string(BootTargetUefiHTTP),
			"BootSourceOverrideEnabled": string(BootContinuous),
			"BootSourceOverrideMode":    string(BootModeUEFI),
			"HttpBootUri":               bootURL,
		},
	}

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish set HTTP boot override", "device", c.deviceID, "bootURL", bootURL)

	return nil
}

// GetBIOSHTTPBootURI returns the pending BIOS UEFI HTTP boot URI.
// Returns ErrUnsupported if the BMC does not support BIOS settings.
func (c *Client) GetBIOSHTTPBootURI(ctx context.Context) (string, error) {
	path := fmt.Sprintf("/redfish/v1/Systems/%s/Bios/Settings", c.deviceID)

	data, status, err := c.session.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}

	if isUnsupportedStatus(status) {
		return "", redfishResponseError(http.MethodGet, path, status, data, ErrUnsupported)
	}

	if status != http.StatusOK {
		return "", redfishResponseError(http.MethodGet, path, status, data, nil)
	}

	var result struct {
		Attributes struct {
			HTTPBootURI string `json:"UrlBootFile"`
		} `json:"Attributes"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing BIOS HTTP boot URI: %w", err)
	}

	slog.Info("Redfish read BIOS HTTP boot URI", "device", c.deviceID, "bootURI", result.Attributes.HTTPBootURI)

	return result.Attributes.HTTPBootURI, nil
}

// SetBIOSHTTPBootURI sets the pending BIOS UEFI HTTP boot URI.
// Returns ErrUnsupported if the BMC does not support BIOS settings.
func (c *Client) SetBIOSHTTPBootURI(ctx context.Context, bootURL string) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s/Bios/Settings", c.deviceID)
	body := map[string]any{
		"Attributes": map[string]string{
			biosHTTPBootURIAttribute: bootURL,
		},
	}

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish set BIOS HTTP boot URI", "device", c.deviceID, "bootURL", bootURL)

	return nil
}

// SetBIOSStaticIPv4 sets pending BIOS UEFI HTTP boot IPv4 settings.
// Returns ErrUnsupported if the BMC does not support BIOS settings.
func (c *Client) SetBIOSStaticIPv4(ctx context.Context, config StaticIPv4Config) error {
	if err := ValidateStaticIPv4Config(config); err != nil {
		return err
	}

	path := fmt.Sprintf("/redfish/v1/Systems/%s/Bios/Settings", c.deviceID)

	attributes := map[string]string{
		biosDHCPv4Attribute:         biosDHCPv4DisabledValue,
		biosIPv4AddressAttribute:    config.Address,
		biosIPv4SubnetMaskAttribute: config.SubnetMask,
		biosIPv4GatewayAttribute:    config.Gateway,
	}
	for _, dns := range config.DNS {
		if net.ParseIP(dns).To4() != nil {
			attributes[biosIPv4PrimaryDNSAttribute] = dns

			break
		}
	}

	body := map[string]any{
		"Attributes": attributes,
	}

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish set BIOS static IPv4", "device", c.deviceID, "address", config.Address)

	return nil
}

// SetStaticIPv4 configures a host EthernetInterface with static IPv4 settings.
// The interface is selected by MACAddress or PermanentMACAddress.
// Returns ErrUnsupported if the BMC does not expose writable EthernetInterface resources.
func (c *Client) SetStaticIPv4(ctx context.Context, config StaticIPv4Config) error {
	mac, err := normalizeMAC(config.MAC)
	if err != nil {
		return err
	}

	if err := ValidateStaticIPv4Config(config); err != nil {
		return err
	}

	path, err := c.findEthernetInterfacePath(ctx, mac)
	if err != nil {
		return err
	}

	address := map[string]string{
		"Address":    config.Address,
		"SubnetMask": config.SubnetMask,
	}
	if config.Gateway != "" {
		address["Gateway"] = config.Gateway
	}

	body := map[string]any{
		"DHCPv4": map[string]bool{
			"DHCPEnabled": false,
		},
		"IPv4StaticAddresses": []map[string]string{address},
	}
	if len(config.DNS) > 0 {
		body["StaticNameServers"] = config.DNS
	}

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish set static IPv4", "device", c.deviceID, "mac", mac, "address", config.Address, "path", path)

	return nil
}

// DisableBootOverride disables the boot source override.
// Returns ErrUnsupported if the BMC does not support disabling.
func (c *Client) DisableBootOverride(ctx context.Context) error {
	path := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	body := map[string]any{
		"Boot": map[string]string{
			"BootSourceOverrideEnabled": string(BootDisabled),
		},
	}

	data, status, err := c.session.do(ctx, http.MethodPatch, path, body)
	if err != nil {
		return err
	}

	if isUnsupportedStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, ErrUnsupported)
	}

	if !isSuccessStatus(status) {
		return redfishResponseError(http.MethodPatch, path, status, data, nil)
	}

	slog.Info("Redfish disabled boot override", "device", c.deviceID)

	return nil
}

func (c *Client) findEthernetInterfacePath(ctx context.Context, mac string) (string, error) {
	systemPath := fmt.Sprintf("/redfish/v1/Systems/%s", c.deviceID)

	data, status, err := c.session.do(ctx, http.MethodGet, systemPath, nil)
	if err != nil {
		return "", err
	}

	if status != http.StatusOK {
		return "", redfishResponseError(http.MethodGet, systemPath, status, data, nil)
	}

	var system struct {
		EthernetInterfaces struct {
			ODataID string `json:"@odata.id"`
		} `json:"EthernetInterfaces"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return "", fmt.Errorf("parsing system EthernetInterfaces link: %w", err)
	}

	collectionPath, err := redfishPathFromODataID(system.EthernetInterfaces.ODataID, systemPath)
	if err != nil {
		return "", fmt.Errorf("system EthernetInterfaces link missing: %w", ErrUnsupported)
	}

	data, status, err = c.session.do(ctx, http.MethodGet, collectionPath, nil)
	if err != nil {
		return "", err
	}

	if isUnsupportedStatus(status) {
		return "", redfishResponseError(http.MethodGet, collectionPath, status, data, ErrUnsupported)
	}

	if status != http.StatusOK {
		return "", redfishResponseError(http.MethodGet, collectionPath, status, data, nil)
	}

	var collection struct {
		Members []struct {
			ODataID string `json:"@odata.id"`
		} `json:"Members"`
	}
	if err := json.Unmarshal(data, &collection); err != nil {
		return "", fmt.Errorf("parsing EthernetInterfaces collection: %w", err)
	}

	for _, member := range collection.Members {
		interfacePath, err := redfishPathFromODataID(member.ODataID, collectionPath)
		if err != nil {
			return "", fmt.Errorf("parsing EthernetInterface member link: %w", err)
		}

		data, status, err = c.session.do(ctx, http.MethodGet, interfacePath, nil)
		if err != nil {
			return "", err
		}

		if status != http.StatusOK {
			return "", redfishResponseError(http.MethodGet, interfacePath, status, data, nil)
		}

		var ethernetInterface struct {
			MACAddress          string `json:"MACAddress"`
			PermanentMACAddress string `json:"PermanentMACAddress"`
		}
		if err := json.Unmarshal(data, &ethernetInterface); err != nil {
			return "", fmt.Errorf("parsing EthernetInterface %s: %w", interfacePath, err)
		}

		if macMatches(mac, ethernetInterface.MACAddress) || macMatches(mac, ethernetInterface.PermanentMACAddress) {
			return interfacePath, nil
		}
	}

	return "", fmt.Errorf("no Redfish Ethernet interface found for MAC %s", mac)
}

// ValidateStaticIPv4Config validates the IP fields in a static network configuration.
func ValidateStaticIPv4Config(config StaticIPv4Config) error {
	if err := validateIPv4Field("address", config.Address, true); err != nil {
		return err
	}

	if err := validateIPv4Field("subnet mask", config.SubnetMask, true); err != nil {
		return err
	}

	if err := validateIPv4Field("gateway", config.Gateway, false); err != nil {
		return err
	}

	for _, dns := range config.DNS {
		if err := validateIPField("DNS server", dns); err != nil {
			return err
		}
	}

	return nil
}

func validateIPv4Field(name, value string, required bool) error {
	if value == "" && !required {
		return nil
	}

	if ip := net.ParseIP(value); ip == nil || ip.To4() == nil {
		return fmt.Errorf("invalid static IPv4 %s %q", name, value)
	}

	return nil
}

func validateIPField(name, value string) error {
	if ip := net.ParseIP(value); ip == nil {
		return fmt.Errorf("invalid IP %s %q", name, value)
	}

	return nil
}

func macMatches(target, candidate string) bool {
	candidate, err := normalizeMAC(candidate)
	if err != nil {
		return false
	}

	return candidate == target
}

func normalizeMAC(value string) (string, error) {
	mac, err := net.ParseMAC(value)
	if err != nil {
		return "", fmt.Errorf("invalid MAC address %q: %w", value, err)
	}

	return mac.String(), nil
}

func redfishPathFromODataID(id, basePath string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", fmt.Errorf("empty @odata.id")
	}

	u, err := url.Parse(id)
	if err != nil {
		return "", err
	}

	if !u.IsAbs() && !strings.HasPrefix(id, "/") {
		base, err := url.Parse(basePath)
		if err != nil {
			return "", err
		}

		u = base.ResolveReference(u)
	}

	path := u.EscapedPath()
	if path == "" {
		return "", fmt.Errorf("empty @odata.id path")
	}

	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}

	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return path, nil
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

	fingerprint := formatFingerprint(sha256Sum(resp.TLS.PeerCertificates[0].Raw))

	slog.Info("Redfish captured TLS cert fingerprint", "url", url, "fingerprint", fingerprint)

	return fingerprint, nil
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
		return "", redfishResponseError(http.MethodGet, "/redfish/v1/Systems", status, data, nil)
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
