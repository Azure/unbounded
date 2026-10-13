// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotReady means a blade is waking after an AC power-on request.
// The caller must retry the original operation. It has not completed yet.
var ErrNotReady = errors.New("BMC is not ready after AC power on")

const (
	rackManagerPath    = "/redfish/v1/Managers/RackManager"
	rackSystemPath     = "/redfish/v1/System"
	rackRecoveryGrace  = 10 * time.Minute
	rackRequestTimeout = 3 * time.Minute
)

type rackSCM struct {
	root             *bmcSession
	slot             string
	mu               sync.Mutex
	now              func() time.Time
	attempted        bool
	deadline         time.Time
	recoveryError    error
	pending          string
	pendingCompleted bool
}

func detectRackSCM(ctx context.Context, blade *bmcSession) (*rackSCM, error) {
	u, err := url.Parse(blade.baseURL)
	if err != nil {
		return nil, fmt.Errorf("parsing BMC URL: %w", err)
	}

	if strings.Trim(u.Path, "/") == "" {
		return nil, nil
	}

	rootURL := *u
	rootURL.Path = ""
	rootURL.RawPath = ""
	rootURL.RawQuery = ""
	rootURL.Fragment = ""
	root := &bmcSession{httpClient: blade.httpClient, baseURL: rootURL.String(), user: blade.user, pass: blade.pass}

	data, status, err := root.do(ctx, http.MethodGet, rackManagerPath, nil)
	if err != nil && (ctx.Err() != nil || errors.Is(err, errTLSCertPin)) {
		return nil, fmt.Errorf("detecting rack manager: %w", err)
	}

	if status != http.StatusOK {
		return nil, nil
	}

	var manager struct {
		ID      string `json:"Id"`
		ODataID string `json:"@odata.id"`
		Oem     struct {
			Microsoft struct {
				Type string `json:"@odata.type"`
			} `json:"Microsoft"`
		} `json:"Oem"`
	}
	if err := json.Unmarshal(data, &manager); err != nil {
		return nil, nil
	}

	if manager.ID != "RackManager" || manager.ODataID != rackManagerPath ||
		!strings.HasPrefix(manager.Oem.Microsoft.Type, "#OcsRackManager.") ||
		!strings.HasSuffix(manager.Oem.Microsoft.Type, ".RackManager") {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("detecting rack manager: %w", err)
	}

	slot := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")

	n, err := strconv.Atoi(slot)
	if err != nil || n < 1 || n > 48 || strconv.Itoa(n) != slot || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("RSCM URL path must be a slot from /1 to /48, got %q", u.EscapedPath())
	}

	// Synchronous TFTP transfers and remote-drive mounts can exceed 30 seconds.
	// Copy the client to keep the pinned transport without changing detection's timeout.
	httpClient := *blade.httpClient
	httpClient.Timeout = rackRequestTimeout
	blade.httpClient = &httpClient
	root.httpClient = &httpClient

	return &rackSCM{root: root, slot: slot, now: time.Now}, nil
}

// rackRequest rejects malformed successful responses before recovery can hide them.
func rackRequest(ctx context.Context, session *bmcSession, method, path string, body any) ([]byte, error) {
	data, status, err := session.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}

	if !isSuccessStatus(status) || (method == http.MethodGet && status != http.StatusOK) {
		return nil, redfishResponseError(method, path, status, data, checkCompletion(data))
	}

	if len(bytes.TrimSpace(data)) == 0 && method != http.MethodGet {
		return data, nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("parsing RSCM %s %s: %w", method, path, err)
	}

	if object == nil {
		return nil, fmt.Errorf("RSCM %s %s returned no object", method, path)
	}

	if raw, ok := object["error"]; ok {
		var result struct {
			Info []completionMessage `json:"@Message.ExtendedInfo"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Info) == 0 {
			return nil, redfishResponseError(method, path, status, data, nil)
		}

		for _, message := range result.Info {
			if message.Oem.Microsoft.CompletionCode != "Success" {
				return nil, redfishResponseError(method, path, status, data, nil)
			}
		}
	}

	return data, nil
}

func (r *rackSCM) power(ctx context.Context) (PowerState, error) {
	data, err := rackRequest(ctx, r.root, http.MethodGet, rackManagerPath+"/SystemPower", nil)
	if err != nil {
		return "", err
	}

	var result struct {
		States map[string]PowerState `json:"System Power States"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", fmt.Errorf("parsing rack system power: %w", err)
	}

	state := result.States[r.slot]
	if state != PowerOff && state != PowerOn {
		return "", fmt.Errorf("invalid rack power state %q for slot %s", state, r.slot)
	}

	return state, nil
}

func (c *Client) rackPowerState(ctx context.Context) (PowerState, error) {
	r := c.rscm
	r.mu.Lock()
	defer r.mu.Unlock()

	data, bladeErr := rackRequest(ctx, c.session, http.MethodGet, rackSystemPath, nil)
	if bladeErr == nil {
		var result struct{ PowerState PowerState }
		if err := json.Unmarshal(data, &result); err != nil {
			return "", fmt.Errorf("parsing blade power state: %w", err)
		}

		if result.PowerState != PowerOn && result.PowerState != PowerOff {
			return "", fmt.Errorf("invalid blade power state %q", result.PowerState)
		}

		return result.PowerState, nil
	}

	if !rackUnavailable(bladeErr) {
		return "", bladeErr
	}

	state, err := r.power(ctx)
	if err != nil {
		return "", fmt.Errorf("blade read failed (%v); rack power: %w", bladeErr, err)
	}

	if state == PowerOff {
		return PowerOff, nil
	}

	// Firmware reports host power in the rack map, but On does not prove BMC
	// readiness. Require a blade response before reporting PowerOn.
	return "", r.readiness(bladeErr)
}

func (c *Client) rackBootConfig(ctx context.Context) (BootConfig, error) {
	r := c.rscm
	r.mu.Lock()
	defer r.mu.Unlock()

	data, err := rackRequest(ctx, c.session, http.MethodGet, rackSystemPath, nil)
	if err != nil {
		if rackUnavailable(err) {
			return BootConfig{}, r.readiness(err)
		}

		return BootConfig{}, err
	}

	var result struct {
		Boot struct {
			Target BootTarget `json:"BootSourceOverrideTarget"`
			Mode   BootMode   `json:"BootSourceOverrideMode"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return BootConfig{}, fmt.Errorf("parsing blade boot config: %w", err)
	}

	switch result.Boot.Target {
	case "None", BootTargetPxe, BootTargetHdd, "RemoteDrive", "BiosSetup":
	default:
		return BootConfig{}, fmt.Errorf("invalid blade boot target %q", result.Boot.Target)
	}

	return BootConfig{Target: result.Boot.Target, Mode: result.Boot.Mode}, nil
}

func (c *Client) rackSetBoot(ctx context.Context, target string) error {
	// SystemPowerOn also powers on the host. Never use it during boot setup;
	// only disabling the override may recover power here.
	return c.rackMutate(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": target}}, target == "None")
}

func (c *Client) rackMutate(ctx context.Context, method, path string, body any, restore bool) error {
	r := c.rscm
	r.mu.Lock()
	defer r.mu.Unlock()

	return c.rackMutateLocked(ctx, method, path, body, restore)
}

// rackMutateLocked requires the rack mutex to be held.
func (c *Client) rackMutateLocked(ctx context.Context, method, path string, body any, restore bool) error {
	r := c.rscm

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	operation := method + path + string(payload)

	r.rearmCompletedRecovery()

	_, bladeErr := rackRequest(ctx, c.session, method, path, body)
	if bladeErr == nil {
		// Keep the fixed wake window even if one command works before reads do.
		if r.pending == operation {
			r.pendingCompleted = true
			r.rearmCompletedRecovery()
		}

		return nil
	}

	if !rackUnavailable(bladeErr) {
		return bladeErr
	}

	if !restore {
		return r.readiness(bladeErr)
	}

	state, err := r.power(ctx)
	if err != nil {
		return fmt.Errorf("blade command failed (%v); rack power: %w", bladeErr, err)
	}

	if state == PowerOff && !r.attempted {
		// Latch before sending. An ambiguous response must not cause repeated AC writes.
		r.attempted = true
		r.pending = operation
		r.deadline = r.now().Add(rackRecoveryGrace)

		_, err := rackRequest(ctx, r.root, http.MethodPost, rackManagerPath+"/Actions/Oem/Microsoft.SystemPowerOn",
			map[string]string{"system_id": r.slot})
		if err != nil {
			r.recoveryError = fmt.Errorf("restoring AC power for slot %s: %w", r.slot, err)
			return r.recoveryError
		}
	}

	return r.readiness(bladeErr)
}

// rearmCompletedRecovery runs with mu held. Failed or incomplete episodes stay latched.
func (r *rackSCM) rearmCompletedRecovery() {
	if !r.pendingCompleted || r.recoveryError != nil || r.now().Before(r.deadline) {
		return
	}

	r.attempted = false
	r.deadline = time.Time{}
	r.pending = ""
	r.pendingCompleted = false
}

// readiness is called with mu held. The deadline never moves on a retry.
func (r *rackSCM) readiness(cause error) error {
	if r.recoveryError != nil {
		return r.recoveryError
	}

	if !r.deadline.IsZero() && r.now().Before(r.deadline) {
		return fmt.Errorf("slot %s: %w: %v", r.slot, ErrNotReady, cause)
	}

	if r.attempted {
		return fmt.Errorf("slot %s AC recovery grace expired: %w", r.slot, cause)
	}

	return cause
}

// Only availability failures may trigger power recovery or readiness retries.
func rackUnavailable(err error) bool {
	var response *responseError
	if !errors.As(err, &response) {
		var networkError net.Error
		return !errors.Is(err, context.Canceled) && errors.As(err, &networkError) && networkError.Timeout()
	}

	if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
		return false
	}

	for _, denied := range []string{"auth", "permission", "privilege", "denied", "invalid", "malformed", "password"} {
		if strings.Contains(strings.ToLower(string(response.body)), denied) {
			return false
		}
	}

	var failure *completionFailure
	if errors.As(err, &failure) {
		description := strings.ToLower(failure.description)
		if description == "" {
			return true
		}

		for _, unavailable := range []string{"not ready", "unavailable", "not available", "not reachable", "unreachable", "timed out", "timeout", "failed to communicate", "failed to query", "failed to discover", "power off", "powered off"} {
			if strings.Contains(description, unavailable) {
				return true
			}
		}

		return false
	}

	return response.cause == nil && (response.status == http.StatusNotFound ||
		response.status == http.StatusInternalServerError || response.status == http.StatusBadGateway ||
		response.status == http.StatusServiceUnavailable || response.status == http.StatusGatewayTimeout)
}
