// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Standard Redfish requests use advertised links, ETags, and task monitors.
// Rack-manager requests do not pass through these helpers.
const (
	taskTimeout         = 5 * time.Minute
	maxTaskPollInterval = 30 * time.Second
)

var taskPollInterval = time.Second

type redfishLink struct {
	ODataID string `json:"@odata.id"`
}

type redfishAction struct {
	Target     string `json:"target"`
	ActionInfo string `json:"@Redfish.ActionInfo"`
}

type actionParameter struct {
	Name            string   `json:"Name"`
	Required        bool     `json:"Required"`
	AllowableValues []string `json:"AllowableValues"`
}

func standardHeader(etag string) http.Header {
	header := http.Header{}
	header.Set("OData-Version", "4.0")
	header.Set("Accept", "application/json")

	if etag != "" {
		header.Set("If-Match", etag)
	}

	return header
}

// Action targets retain their advertised trailing slash.
func actionPath(target, basePath string) (string, error) {
	return redfishPathFromODataID(target, basePath)
}

func resourcePath(id, basePath string) (string, error) {
	return redfishPathFromODataID(id, basePath)
}

// Normalize request paths without changing the base used to resolve links.
func resourceRequestPath(path string) string {
	path, query, hasQuery := strings.Cut(path, "?")

	if trimmed := strings.TrimRight(path, "/"); trimmed != "" {
		path = trimmed
	}

	if hasQuery {
		path += "?" + query
	}

	return path
}

func (c *Client) system() string {
	return resourceRequestPath(c.systemBase())
}

func (c *Client) systemBase() string {
	if c.systemPath != "" {
		return c.systemPath
	}

	return "/redfish/v1/Systems/" + c.deviceID
}

func (c *Client) getResource(ctx context.Context, path string) ([]byte, string, error) {
	path = resourceRequestPath(path)

	resp, err := c.session.send(ctx, http.MethodGet, path, nil, standardHeader(""))
	if err != nil {
		return nil, "", err
	}

	if resp.status != http.StatusOK {
		var cause error
		if resp.status == http.StatusNotFound || resp.status == http.StatusMethodNotAllowed ||
			resp.status == http.StatusGone || resp.status == http.StatusNotImplemented {
			cause = ErrUnsupported
		}

		return nil, "", redfishResponseError(http.MethodGet, path, resp.status, resp.data, cause)
	}

	return resp.data, resourceETag(resp), nil
}

func resourceETag(resp sessionResponse) string {
	if etag := resp.header.Get("ETag"); etag != "" {
		return etag
	}

	var body struct {
		ETag string `json:"@odata.etag"`
	}
	if json.Unmarshal(resp.data, &body) == nil {
		return body.ETag
	}

	return ""
}

// modify retries a PATCH once with a fresh ETag when required, then waits for
// an asynchronous operation's task monitor, if one is returned.
func (c *Client) modify(ctx context.Context, method, path string, body any) (sessionResponse, error) {
	resp, err := c.session.send(ctx, method, path, body, standardHeader(""))
	if err != nil {
		return resp, err
	}

	if method == http.MethodPatch &&
		(resp.status == http.StatusPreconditionRequired || resp.status == http.StatusPreconditionFailed) {
		_, etag, err := c.getResource(ctx, path)
		if err != nil {
			return resp, err
		}

		if etag == "" {
			return resp, redfishResponseError(method, path, resp.status, resp.data,
				errors.New("service requires If-Match but did not return an ETag"))
		}

		resp, err = c.session.send(ctx, method, path, body, standardHeader(etag))
		if err != nil {
			return resp, err
		}
	}

	if resp.status == http.StatusAccepted {
		return c.waitTask(ctx, method, path, resp)
	}

	return resp, nil
}

func (c *Client) waitTask(ctx context.Context, method, path string, accepted sessionResponse) (sessionResponse, error) {
	location := accepted.header.Get("Location")
	if location == "" {
		var task redfishLink
		if json.Unmarshal(accepted.data, &task) == nil {
			location = task.ODataID
		}
	}

	if location == "" {
		return accepted, nil
	}

	monitor, err := redfishPathFromODataID(location, path)
	if err != nil {
		return accepted, fmt.Errorf("parsing redfish task monitor %q: %w", location, err)
	}

	ctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()

	resp := accepted

	for {
		timer := time.NewTimer(retryAfter(resp.header))

		select {
		case <-ctx.Done():
			timer.Stop()
			return resp, fmt.Errorf("waiting for redfish %s %s task %s: %w", method, path, monitor, ctx.Err())
		case <-timer.C:
		}

		resp, err = c.session.send(ctx, http.MethodGet, monitor, nil, standardHeader(""))
		if err != nil {
			return resp, err
		}

		if resp.status == http.StatusAccepted {
			continue
		}

		done, err := taskFinished(resp)
		if err != nil {
			return resp, fmt.Errorf("redfish %s %s task %s: %w", method, path, monitor, err)
		}

		if done {
			return resp, nil
		}
	}
}

func taskFinished(resp sessionResponse) (bool, error) {
	if !isSuccessStatus(resp.status) {
		return true, actionError(http.MethodGet, "task monitor", resp)
	}

	var task struct {
		TaskState  string `json:"TaskState"`
		TaskStatus string `json:"TaskStatus"`
	}
	if json.Unmarshal(resp.data, &task) != nil || task.TaskState == "" {
		return true, nil
	}

	switch task.TaskState {
	case "Completed":
		if task.TaskStatus == "Critical" {
			return true, fmt.Errorf("task completed with status Critical: %s", resp.data)
		}

		return true, nil
	case "Exception", "Killed", "Cancelled", "Canceled", "Interrupted": //nolint:misspell // Redfish wire value.
		return true, fmt.Errorf("task ended in state %s: %s", task.TaskState, resp.data)
	default:
		return false, nil
	}
}

func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return taskPollInterval
	}

	return time.Duration(min(seconds, int(maxTaskPollInterval/time.Second))) * time.Second
}

func licenseRequired(data []byte) bool {
	var body struct {
		Error struct {
			Code         string `json:"code"`
			ExtendedInfo []struct {
				MessageID string `json:"MessageId"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
		ExtendedInfo []struct {
			MessageID string `json:"MessageId"`
		} `json:"@Message.ExtendedInfo"`
	}
	if json.Unmarshal(data, &body) != nil {
		return false
	}

	isLicense := func(messageID string) bool {
		parts := strings.Split(messageID, ".")
		switch parts[len(parts)-1] {
		case "LicenseKeyRequired", "LicenseRequired", "OemLicenseNotPassed":
			return true
		default:
			return false
		}
	}

	if isLicense(body.Error.Code) {
		return true
	}

	for _, info := range append(body.Error.ExtendedInfo, body.ExtendedInfo...) {
		if isLicense(info.MessageID) {
			return true
		}
	}

	return false
}

func actionError(method, path string, resp sessionResponse) error {
	var cause error

	switch {
	case licenseRequired(resp.data):
		cause = fmt.Errorf("BMC license does not permit this operation: %w", ErrUnsupported)
	case resp.status == http.StatusMethodNotAllowed || resp.status == http.StatusNotImplemented:
		cause = ErrUnsupported
	}

	return redfishResponseError(method, path, resp.status, resp.data, cause)
}

func (c *Client) actionParameters(ctx context.Context, action redfishAction, basePath string) ([]actionParameter, error) {
	if action.ActionInfo == "" {
		return nil, nil
	}

	path, err := resourcePath(action.ActionInfo, basePath)
	if err != nil {
		return nil, fmt.Errorf("parsing ActionInfo link: %w", err)
	}

	data, _, err := c.getResource(ctx, path)
	if err != nil {
		return nil, err
	}

	var info struct {
		Parameters []actionParameter `json:"Parameters"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("parsing ActionInfo %s: %w", path, err)
	}

	if info.Parameters == nil {
		info.Parameters = []actionParameter{}
	}

	return info.Parameters, nil
}

func (c *Client) resetTarget(ctx context.Context, resetType ResetType) (string, error) {
	path := c.system()
	base := c.systemBase()

	data, _, err := c.getResource(ctx, path)
	if errors.Is(err, ErrUnsupported) {
		return path + "/Actions/ComputerSystem.Reset", nil
	}

	if err != nil {
		return "", err
	}

	var system struct {
		redfishLink
		Actions struct {
			Reset *struct {
				redfishAction
				AllowableValues []ResetType `json:"ResetType@Redfish.AllowableValues"`
			} `json:"#ComputerSystem.Reset"`
		} `json:"Actions"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return "", fmt.Errorf("parsing system actions: %w", err)
	}

	if system.ODataID != "" {
		base, err = resourcePath(system.ODataID, base)
		if err != nil {
			return "", fmt.Errorf("parsing system link: %w", err)
		}
	}

	reset := system.Actions.Reset
	if reset == nil || reset.Target == "" {
		return path + "/Actions/ComputerSystem.Reset", nil
	}

	allowed := reset.AllowableValues
	if allowed == nil {
		params, err := c.actionParameters(ctx, reset.redfishAction, base)
		if err != nil {
			return "", err
		}

		for _, param := range params {
			if param.Name == "ResetType" {
				for _, value := range param.AllowableValues {
					allowed = append(allowed, ResetType(value))
				}
			}
		}
	}

	if len(allowed) > 0 && !slices.Contains(allowed, resetType) {
		return "", fmt.Errorf("reset type %s is not allowed by the BMC: %w", resetType, ErrUnsupported)
	}

	return actionPath(reset.Target, base)
}

type systemBootSettings struct {
	Target         BootTarget
	Enabled        BootEnabled
	Mode           BootMode
	HasMode        bool
	AllowedTargets []BootTarget
}

func (c *Client) systemBoot(ctx context.Context) (systemBootSettings, error) {
	data, _, err := c.getResource(ctx, c.system())
	if err != nil {
		return systemBootSettings{}, err
	}

	var system struct {
		Boot struct {
			Target         BootTarget   `json:"BootSourceOverrideTarget"`
			Enabled        BootEnabled  `json:"BootSourceOverrideEnabled"`
			Mode           *BootMode    `json:"BootSourceOverrideMode"`
			AllowedTargets []BootTarget `json:"BootSourceOverrideTarget@Redfish.AllowableValues"`
		} `json:"Boot"`
	}
	if err := json.Unmarshal(data, &system); err != nil {
		return systemBootSettings{}, fmt.Errorf("parsing system boot config: %w", err)
	}

	boot := systemBootSettings{
		Target: system.Boot.Target, Enabled: system.Boot.Enabled,
		HasMode: system.Boot.Mode != nil, AllowedTargets: system.Boot.AllowedTargets,
	}
	if system.Boot.Mode != nil {
		boot.Mode = *system.Boot.Mode
	}

	return boot, nil
}

func (c *Client) patchBoot(ctx context.Context, boot map[string]string) error {
	path := c.system()

	resp, err := c.modify(ctx, http.MethodPatch, path, map[string]any{"Boot": boot})
	if err != nil {
		return err
	}

	if licenseRequired(resp.data) {
		return actionError(http.MethodPatch, path, resp)
	}

	if isUnsupportedStatus(resp.status) {
		return redfishResponseError(http.MethodPatch, path, resp.status, resp.data, ErrUnsupported)
	}

	if !isSuccessStatus(resp.status) {
		return actionError(http.MethodPatch, path, resp)
	}

	return nil
}
