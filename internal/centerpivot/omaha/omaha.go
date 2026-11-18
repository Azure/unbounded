// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package omaha sends Nebraska an update check on a node's behalf. The check
// is shaped exactly like trident-acl-agent's own (its core/nebraska/wire.rs),
// so Nebraska sees one instance per node, whichever of the two sends it.
//
// It only ever sends an update check: never a ping or an event, which belong
// to the agent. In Nebraska, an update check that offers an update is a grant
// of that update to the node.
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
// it, and the server, application, and track to check.
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
	body, err := requestBody(check)
	if err != nil {
		return Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, check.Server, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build update check: %w", err)
	}

	req.Header.Set("Content-Type", "application/xml")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("send update check: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // Best-effort close of HTTP response body.

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return Result{}, fmt.Errorf("update check: HTTP %d", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return Result{}, fmt.Errorf("read update check response: %w", err)
	}

	return interpret(data, check.AppID)
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
		AppID       string   `xml:"appid,attr"`
		Version     string   `xml:"version,attr"`
		Track       string   `xml:"track,attr"`
		MachineID   string   `xml:"machineid,attr"`
		UpdateCheck struct{} `xml:"updatecheck"`
	} `xml:"app"`
}

// requestBody returns the check as the agent writes it: a request with a new
// session ID, the os element carrying the app's version, and one app with an
// updatecheck.
func requestBody(check Check) ([]byte, error) {
	if check.Server == "" || check.AppID == "" || check.Track == "" || check.MachineID == "" || check.Version == "" {
		return nil, errors.New("update check needs a server, app ID, track, machine ID, and version")
	}

	if check.Arch != "amd64" && check.Arch != "aarch64" {
		return nil, fmt.Errorf("update check architecture %q isn't amd64 or aarch64", check.Arch)
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

	data, err := xml.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("encode update check: %w", err)
	}

	return append([]byte(xml.Header), data...), nil
}

type response struct {
	Apps []struct {
		AppID       string `xml:"appid,attr"`
		Status      string `xml:"status,attr"`
		UpdateCheck *struct {
			Status   string `xml:"status,attr"`
			Manifest *struct {
				Version string `xml:"version,attr"`
			} `xml:"manifest"`
		} `xml:"updatecheck"`
	} `xml:"app"`
}

// interpret reads Nebraska's answer as the agent does (interpret_check in its
// core/nebraska/client.rs).
func interpret(data []byte, appID string) (Result, error) {
	var resp response
	if err := xml.Unmarshal(data, &resp); err != nil {
		return Result{}, fmt.Errorf("decode update check response: %w", err)
	}

	if len(resp.Apps) != 1 || !strings.EqualFold(resp.Apps[0].AppID, appID) {
		return Result{}, fmt.Errorf("update check response doesn't have exactly one app %s", appID)
	}

	app := resp.Apps[0]

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
