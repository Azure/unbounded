// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package omaha talks to Nebraska on a node's behalf. Its requests are shaped
// exactly like trident-acl-agent's own (its core/nebraska/wire.rs), so
// Nebraska sees one instance per node, whichever of the two sends them.
//
// It sends two requests: an update check, which in Nebraska grants the node
// an update when there is one, and the agent's report that an update failed,
// which ends that grant. Pings and progress events belong to the agent.
package omaha

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
)

const (
	protocolVersion = "3.0"
	// statusUpdateInProgress is Nebraska's answer while the node has an update
	// that started downloading and hasn't finished or failed.
	statusUpdateInProgress = "error-updateInProgressOnInstance"
	maxResponseBytes       = 1 << 20
)

// Check is one node's update check: the node's identity as the agent reports
// it, and the server, application, and track to check. ReportFailure takes
// the check that took the grant it ends.
type Check struct {
	// Server is the Nebraska update endpoint, such as
	// https://nebraska.example.com/v1/update.
	Server string
	AppID  string
	Track  string
	// MachineID is the node's Nebraska instance ID.
	MachineID string
	// Version is the node's current version, as the agent reports it.
	Version string
	// Arch is the architecture, as Nebraska spells it: amd64 or aarch64.
	Arch string
}

// Kind is what Nebraska answered.
type Kind int

const (
	// NoUpdate means there's nothing newer for the node now.
	NoUpdate Kind = iota
	// Granted means Nebraska granted the node an update to Result.Version.
	Granted
	// InProgress means the node already has an update that started
	// downloading and hasn't finished or failed.
	InProgress
	// Refused means Nebraska refused the check, for example because a rollout
	// limit is reached or updates are off; Result.Status says why.
	Refused
)

// Result is Nebraska's answer to an update check.
type Result struct {
	Kind Kind
	// Version is the version granted, when Kind is Granted.
	Version string
	// Status is Nebraska's status, when Kind is Refused.
	Status string
}

// Client sends update checks.
type Client struct {
	// HTTP sends the requests. Its timeout bounds each check.
	HTTP *http.Client
}

// UpdateCheck sends one update check and returns Nebraska's answer.
func (c *Client) UpdateCheck(ctx context.Context, check Check) (Result, error) {
	body, err := requestBody(check, nil)
	if err != nil {
		return Result{}, err
	}

	data, err := c.post(ctx, check.Server, body)
	if err != nil {
		return Result{}, fmt.Errorf("update check: %w", err)
	}

	return interpret(data, check.AppID)
}

// ReportFailure tells Nebraska that the update the check's grant started
// failed, as the agent does (report_failure in its core/nebraska/client.rs).
// That ends the grant: the node is no longer counted as updating, and its
// next check may be granted again.
//
// Nebraska records the report only while the node has an update in progress,
// from the grant until the update finishes or fails, and acknowledges it
// either way. So a report for a grant already ended does nothing, and an
// acknowledgment confirms only that Nebraska knew the app and track.
func (c *Client) ReportFailure(ctx context.Context, check Check) error {
	body, err := requestBody(check, &event{Type: eventTypeUpdateComplete, Result: eventResultError, PreviousVersion: check.Version})
	if err != nil {
		return err
	}

	data, err := c.post(ctx, check.Server, body)
	if err != nil {
		return fmt.Errorf("report a failed update: %w", err)
	}

	return acknowledged(data, check.AppID)
}

func (c *Client) post(ctx context.Context, server string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return data, nil
}

// The Omaha event that reports a failed update: update complete, with an
// error result.
const (
	eventTypeUpdateComplete = "3"
	eventResultError        = "0"
)

type event struct {
	Type            string `xml:"eventtype,attr"`
	Result          string `xml:"eventresult,attr"`
	PreviousVersion string `xml:"previousversion,attr"`
}

type request struct {
	XMLName   xml.Name `xml:"request"`
	Protocol  string   `xml:"protocol,attr"`
	IsMachine string   `xml:"ismachine,attr"`
	SessionID string   `xml:"sessionid,attr"`
	OS        struct {
		Platform string `xml:"platform,attr"`
		Version  string `xml:"version,attr"`
		Arch     string `xml:"arch,attr"`
	} `xml:"os"`
	App struct {
		AppID       string    `xml:"appid,attr"`
		Version     string    `xml:"version,attr"`
		Track       string    `xml:"track,attr"`
		MachineID   string    `xml:"machineid,attr"`
		Event       *event    `xml:"event"`
		UpdateCheck *struct{} `xml:"updatecheck"`
	} `xml:"app"`
}

// requestBody returns a request as the agent writes it: a new session ID, the
// os element carrying the app's version, and one app with either an
// updatecheck or, without one, the event.
func requestBody(check Check, ev *event) ([]byte, error) {
	if check.Server == "" || check.AppID == "" || check.Track == "" || check.MachineID == "" || check.Version == "" {
		return nil, errors.New("a request to Nebraska needs a server, app ID, track, machine ID, and version")
	}

	if check.Arch != "amd64" && check.Arch != "aarch64" {
		return nil, fmt.Errorf("architecture %q isn't amd64 or aarch64", check.Arch)
	}

	var r request

	r.Protocol = protocolVersion
	r.IsMachine = "1"
	r.SessionID = uuid.NewString()
	r.OS.Platform = "linux"
	r.OS.Version = check.Version
	r.OS.Arch = check.Arch
	r.App.AppID = check.AppID
	r.App.Version = check.Version
	r.App.Track = check.Track
	r.App.MachineID = check.MachineID

	if ev != nil {
		r.App.Event = ev
	} else {
		r.App.UpdateCheck = &struct{}{}
	}

	data, err := xml.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode the request: %w", err)
	}

	return append([]byte(xml.Header), data...), nil
}

type response struct {
	Apps []appResponse `xml:"app"`
}

type appResponse struct {
	AppID  string `xml:"appid,attr"`
	Status string `xml:"status,attr"`
	Events []struct {
		Status string `xml:"status,attr"`
	} `xml:"event"`
	UpdateCheck *struct {
		Status   string `xml:"status,attr"`
		Manifest *struct {
			Version string `xml:"version,attr"`
		} `xml:"manifest"`
	} `xml:"updatecheck"`
}

// interpret reads Nebraska's answer as the agent does (interpret_check in its
// core/nebraska/client.rs).
func interpret(data []byte, appID string) (Result, error) {
	app, err := appOf(data, appID)
	if err != nil {
		return Result{}, err
	}

	switch app.Status {
	case "ok":
	case statusUpdateInProgress:
		return Result{Kind: InProgress}, nil
	default:
		return Result{Kind: Refused, Status: app.Status}, nil
	}

	if app.UpdateCheck == nil {
		return Result{}, errors.New("update check response has no updatecheck")
	}

	switch app.UpdateCheck.Status {
	case "noupdate":
		return Result{Kind: NoUpdate}, nil
	case "ok":
		if app.UpdateCheck.Manifest == nil || app.UpdateCheck.Manifest.Version == "" {
			return Result{}, errors.New("update check response offers an update without a version")
		}

		return Result{Kind: Granted, Version: app.UpdateCheck.Manifest.Version}, nil
	default:
		return Result{Kind: Refused, Status: app.UpdateCheck.Status}, nil
	}
}

// acknowledged checks that Nebraska acknowledged the event, as the agent does
// (require_events_accepted in its core/nebraska/client.rs). Nebraska skips the
// events of an app or track it doesn't know, and says why in the app's status.
func acknowledged(data []byte, appID string) error {
	app, err := appOf(data, appID)
	if err != nil {
		return err
	}

	for _, ev := range app.Events {
		if ev.Status != "ok" {
			return fmt.Errorf("the event was rejected: %s", ev.Status)
		}
	}

	if len(app.Events) > 0 {
		return nil
	}

	if app.Status != "ok" && app.Status != statusUpdateInProgress {
		return fmt.Errorf("the event was skipped: %s", app.Status)
	}

	return errors.New("the event wasn't acknowledged")
}

// appOf returns the response's one app, which must be appID.
func appOf(data []byte, appID string) (appResponse, error) {
	var resp response
	if err := xml.Unmarshal(data, &resp); err != nil {
		return appResponse{}, fmt.Errorf("decode the response: %w", err)
	}

	if len(resp.Apps) != 1 || !strings.EqualFold(resp.Apps[0].AppID, appID) {
		return appResponse{}, fmt.Errorf("the response doesn't have exactly one app %s", appID)
	}

	return resp.Apps[0], nil
}

// Arch returns Nebraska's spelling of a Node's kubernetes.io/arch label, as the
// agent spells it: Nebraska doesn't know arm64, and would put such a node in an
// amd64 group.
func Arch(kubernetesArch string) (string, error) {
	switch kubernetesArch {
	case "amd64":
		return "amd64", nil
	case "arm64":
		return "aarch64", nil
	default:
		return "", fmt.Errorf("architecture %q isn't supported", kubernetesArch)
	}
}
