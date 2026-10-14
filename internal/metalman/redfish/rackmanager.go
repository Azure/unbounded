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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotReady means a blade is waking after a slot power-on request.
// The caller must retry the original operation. It has not completed yet.
var ErrNotReady = errors.New("BMC is not ready after AC power on")

const (
	rackManagerPath    = "/redfish/v1/Managers/RackManager"
	rackSystemPath     = "/redfish/v1/System"
	rackRecoveryGrace  = 10 * time.Minute
	rackRequestTimeout = 3 * time.Minute
)

// rackManager talks to a rack manager that fronts many blade BMCs. A blade
// is addressed by a slot number placed before /redfish in the URL, for example
// https://manager/7/redfish/v1/System. Rack-wide resources live at the origin
// root under /redfish/v1/Managers/RackManager.
type rackManager struct {
	root             *bmcSession
	slot             string
	powerOnTarget    string
	mu               sync.Mutex
	now              func() time.Time
	deadline         time.Time
	recoveryError    error
	pending          recoveryOp
	pendingCompleted bool
}

type recoveryOp int

const (
	recoveryNone recoveryOp = iota
	recoveryResetOn
	recoveryClearOverride
)

// detectRackManager reports whether an inconclusive probe needs fallback validation.
func detectRackManager(ctx context.Context, blade *bmcSession) (rack *rackManager, inconclusive bool, err error) {
	u, err := url.Parse(blade.baseURL)
	if err != nil {
		return nil, false, fmt.Errorf("parsing BMC URL: %w", err)
	}

	if strings.Trim(u.Path, "/") == "" {
		return nil, false, nil
	}

	rootURL := *u
	rootURL.Path = ""
	rootURL.RawPath = ""
	rootURL.RawQuery = ""
	rootURL.Fragment = ""
	root := &bmcSession{httpClient: blade.httpClient, baseURL: rootURL.String(), user: blade.user, pass: blade.pass}

	data, status, err := root.do(ctx, http.MethodGet, rackManagerPath, nil)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, errTLSCertPin) {
			return nil, false, fmt.Errorf("detecting rack manager: %w", err)
		}

		return nil, true, nil
	}

	if status == http.StatusNotFound || status == http.StatusNotImplemented {
		return nil, false, nil
	}

	if status != http.StatusOK {
		return nil, true, nil
	}

	var manager struct {
		ID      string `json:"Id"`
		ODataID string `json:"@odata.id"`
		Actions struct {
			Oem map[string]struct {
				Target string `json:"target"`
			} `json:"Oem"`
		} `json:"Actions"`
	}
	if err := json.Unmarshal(data, &manager); err != nil {
		return nil, true, nil
	}

	if manager.ID != "RackManager" || manager.ODataID != rackManagerPath {
		return nil, false, nil
	}

	powerOnTarget := slotPowerOnTarget(manager.Actions.Oem)
	if powerOnTarget == "" {
		return nil, false, nil
	}

	slot := strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")

	n, err := strconv.Atoi(slot)
	if err != nil || n < 1 || strconv.Itoa(n) != slot || u.RawPath != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, false, fmt.Errorf("rack manager URL path must be a slot number such as /7, got %q", u.EscapedPath())
	}

	// Synchronous TFTP transfers and remote-drive mounts can exceed 30 seconds.
	// Copy the client to keep the pinned transport without changing detection's timeout.
	httpClient := *blade.httpClient
	httpClient.Timeout = rackRequestTimeout
	blade.httpClient = &httpClient
	root.httpClient = &httpClient

	return &rackManager{root: root, slot: slot, powerOnTarget: powerOnTarget, now: time.Now}, false, nil
}

// slotPowerOnTarget finds the vendor action that powers on one slot. Its key
// ends with "#SystemPower.On", after a vendor schema prefix. The target must
// be under the rack manager.
func slotPowerOnTarget(actions map[string]struct {
	Target string `json:"target"`
},
) string {
	keys := make([]string, 0, len(actions))
	for key := range actions {
		keys = append(keys, key)
	}

	slices.Sort(keys)

	for _, key := range keys {
		target := actions[key].Target
		if strings.HasSuffix(key, "#SystemPower.On") && strings.HasPrefix(target, rackManagerPath+"/") {
			return target
		}
	}

	return ""
}

// rackRequest rejects malformed successful responses before recovery can hide them.
// It also rejects 202 Accepted: the task may still be running, and callers
// such as cleanup must not report success before the work is done.
func rackRequest(ctx context.Context, session *bmcSession, method, path string, body any) ([]byte, error) {
	data, status, err := session.do(ctx, method, path, body)
	if err != nil {
		return nil, err
	}

	if !isSuccessStatus(status) || status == http.StatusAccepted || (method == http.MethodGet && status != http.StatusOK) {
		return nil, redfishResponseError(method, path, status, data, checkCompletion(data))
	}

	if len(bytes.TrimSpace(data)) == 0 && method != http.MethodGet {
		return data, nil
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return nil, fmt.Errorf("parsing rack manager response %s %s: %w", method, path, err)
	}

	if object == nil {
		return nil, fmt.Errorf("rack manager %s %s returned no object", method, path)
	}

	if raw, ok := object["error"]; ok {
		var result struct {
			Info []completionMessage `json:"@Message.ExtendedInfo"`
		}
		if json.Unmarshal(raw, &result) != nil || len(result.Info) == 0 {
			return nil, redfishResponseError(method, path, status, data, nil)
		}

		for _, message := range result.Info {
			if ok, err := message.completion(); err != nil || !ok {
				return nil, redfishResponseError(method, path, status, data, nil)
			}
		}
	}

	return data, nil
}

func (r *rackManager) power(ctx context.Context) (PowerState, error) {
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
	r := c.rack
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

		if result.PowerState == PowerOn && r.pending == recoveryResetOn {
			r.pendingCompleted = true
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
	r := c.rack
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
	op := recoveryNone
	if target == "None" {
		op = recoveryClearOverride
	}

	return c.rackMutate(ctx, http.MethodPatch, rackSystemPath,
		map[string]any{"Boot": map[string]string{"BootSourceOverrideTarget": target}}, op)
}

func (c *Client) rackMutate(ctx context.Context, method, path string, body any, op recoveryOp) error {
	r := c.rack
	r.mu.Lock()
	defer r.mu.Unlock()

	return c.rackMutateLocked(ctx, method, path, body, op)
}

// rackMutateLocked requires the rack mutex to be held.
func (c *Client) rackMutateLocked(ctx context.Context, method, path string, body any, op recoveryOp) error {
	r := c.rack

	r.rearmCompletedRecovery()

	_, bladeErr := rackRequest(ctx, c.session, method, path, body)
	if bladeErr == nil {
		// Keep the fixed wake window even if one command works before reads do.
		if op != recoveryNone && r.pending == op {
			r.pendingCompleted = true
			r.rearmCompletedRecovery()
		}

		return nil
	}

	if !rackUnavailable(bladeErr) {
		return bladeErr
	}

	if op == recoveryNone {
		return r.readiness(bladeErr)
	}

	state, err := r.power(ctx)
	if err != nil {
		return fmt.Errorf("blade command failed (%v); rack power: %w", bladeErr, err)
	}

	if state == PowerOff && r.pending == recoveryNone {
		// Latch before sending. An ambiguous response must not cause repeated AC writes.
		r.pending = op
		r.deadline = r.now().Add(rackRecoveryGrace)

		_, err := rackRequest(ctx, r.root, http.MethodPost, r.powerOnTarget,
			map[string]string{"system_id": r.slot})
		if err != nil {
			r.recoveryError = fmt.Errorf("restoring AC power for slot %s: %w", r.slot, err)
			return r.recoveryError
		}
	}

	return r.readiness(bladeErr)
}

// rearmCompletedRecovery runs with mu held. Failed or incomplete episodes stay latched.
func (r *rackManager) rearmCompletedRecovery() {
	if !r.pendingCompleted || r.recoveryError != nil || r.now().Before(r.deadline) {
		return
	}

	r.deadline = time.Time{}
	r.pending = recoveryNone
	r.pendingCompleted = false
}

// readiness is called with mu held. The deadline never moves on a retry.
func (r *rackManager) readiness(cause error) error {
	if r.recoveryError != nil {
		return r.recoveryError
	}

	if !r.deadline.IsZero() && r.now().Before(r.deadline) {
		return fmt.Errorf("slot %s: %w: %v", r.slot, ErrNotReady, cause)
	}

	if r.pending != recoveryNone {
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
