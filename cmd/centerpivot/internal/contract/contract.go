// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package contract implements trident-acl-agent's Node annotation contract:
// the update request an orchestrator writes, and the statuses the agent writes
// back. See
// https://github.com/microsoft/trident/blob/main/docs/Explanation/Trident-ACL-Agent.md#the-annotation-contract
// and the agent's annotations/protocol.rs, which this package follows.
package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// SchemaVersion is the only schema version the agent accepts.
const SchemaVersion = "1.0"

// Keys are the contract's three annotation keys under one prefix.
type Keys struct {
	Request      string
	Status       string
	CommitStatus string
}

// KeysFor returns the annotation keys under prefix, such as
// centerpivot.unbounded-cloud.io. The agent must be configured with the same
// prefix.
func KeysFor(prefix string) Keys {
	return Keys{
		Request:      prefix + "/update-request",
		Status:       prefix + "/update-status",
		CommitStatus: prefix + "/update-commit-status",
	}
}

// Operation is a requested or reported operation. Requests use stage and
// finalize; the agent reports commit, after the restart, on the commit-status
// annotation.
type Operation string

// The contract's operations.
const (
	OperationStage    Operation = "stage"
	OperationFinalize Operation = "finalize"
	OperationRollback Operation = "rollback"
	OperationCommit   Operation = "commit"
)

// Code is the outcome a status reports.
type Code string

// The contract's status codes. Every code but InProgress is final.
const (
	CodeInProgress         Code = "InProgress"
	CodeSuccess            Code = "Success"
	CodeAlreadyAtTarget    Code = "AlreadyAtTarget"
	CodeNotStaged          Code = "NotStaged"
	CodeOperationFailed    Code = "OperationFailed"
	CodeTargetBootFailed   Code = "TargetBootFailed"
	CodeAgentInternalError Code = "AgentInternalError"
	CodeInvalidRequest     Code = "InvalidRequest"
)

// Final reports whether the code ends its operation.
func (c Code) Final() bool {
	return c != CodeInProgress
}

// Source names where a stage or finalize request gets its image from: the
// Nebraska update endpoint, the application, and the track.
type Source struct {
	Server string
	AppID  string
	Track  string
}

// Request is an update request. The agent rejects a request with any field
// the contract doesn't define, so this type has exactly the contract's fields.
type Request struct {
	SchemaVersion string    `json:"schemaVersion"`
	NodeUpdateID  string    `json:"nodeUpdateId"`
	OperationID   string    `json:"operationId"`
	Operation     Operation `json:"operation"`
	TargetVersion string    `json:"targetVersion,omitempty"`
	Server        string    `json:"server,omitempty"`
	AppID         string    `json:"appId,omitempty"`
	Track         string    `json:"track,omitempty"`
}

// NewRequest returns a stage or finalize request for targetVersion from
// source, with a new operation ID. A stage starts an update, so it gets a new
// update ID; a finalize continues the update nodeUpdateID names.
func NewRequest(operation Operation, nodeUpdateID, targetVersion string, source Source) Request {
	if operation == OperationStage {
		nodeUpdateID = uuid.NewString()
	}

	return Request{
		SchemaVersion: SchemaVersion,
		NodeUpdateID:  nodeUpdateID,
		OperationID:   uuid.NewString(),
		Operation:     operation,
		TargetVersion: targetVersion,
		Server:        source.Server,
		AppID:         source.AppID,
		Track:         source.Track,
	}
}

// Validate checks the request as the agent does before acting on it, so a
// request the agent would answer with InvalidRequest is never written.
func (r Request) Validate() error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schemaVersion %q", r.SchemaVersion)
	}

	if _, err := uuid.Parse(r.NodeUpdateID); err != nil {
		return fmt.Errorf("nodeUpdateId must be a UUID, got %q", r.NodeUpdateID)
	}

	if _, err := uuid.Parse(r.OperationID); err != nil {
		return fmt.Errorf("operationId must be a UUID, got %q", r.OperationID)
	}

	switch r.Operation {
	case OperationStage, OperationFinalize:
	default:
		return fmt.Errorf("operation %q isn't stage or finalize", r.Operation)
	}

	if r.TargetVersion == "" {
		return errors.New("targetVersion is required")
	}

	server, err := url.Parse(r.Server)
	if err != nil || server.Scheme != "https" || server.Host == "" {
		return fmt.Errorf("server must be an https URL, got %q", r.Server)
	}

	if r.AppID == "" || r.Track == "" {
		return errors.New("appId and track are required")
	}

	return nil
}

// Marshal validates the request, and returns it as the annotation's value.
func (r Request) Marshal() (string, error) {
	if err := r.Validate(); err != nil {
		return "", fmt.Errorf("invalid update request: %w", err)
	}

	data, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("encode update request: %w", err)
	}

	return string(data), nil
}

// Status is a status the agent writes. Fields the contract adds later are
// ignored, so a newer agent's statuses still parse.
type Status struct {
	SchemaVersion  string        `json:"schemaVersion"`
	NodeUpdateID   string        `json:"nodeUpdateId"`
	OperationID    string        `json:"operationId"`
	Operation      Operation     `json:"operation"`
	Code           Code          `json:"code"`
	Message        string        `json:"message"`
	TridentError   *TridentError `json:"tridentError,omitempty"`
	FromVersion    string        `json:"fromVersion,omitempty"`
	ToVersion      string        `json:"toVersion,omitempty"`
	TridentVersion string        `json:"tridentVersion,omitempty"`
	StartedUTC     time.Time     `json:"startedUtc"`
	LastUpdatedUTC time.Time     `json:"lastUpdatedUtc"`
	FinishedUTC    *time.Time    `json:"finishedUtc,omitempty"`
}

// TridentError is Trident's classification of a failure, when one came from a
// Trident call.
type TridentError struct {
	Kind     string         `json:"kind"`
	Subkind  string         `json:"subkind"`
	Location *ErrorLocation `json:"location,omitempty"`
}

// ErrorLocation is where in Trident's source a failure was raised.
type ErrorLocation struct {
	Path string `json:"path"`
	Line uint32 `json:"line"`
}

// ParseStatus parses a status annotation's value.
func ParseStatus(value string) (Status, error) {
	var status Status
	if err := json.Unmarshal([]byte(value), &status); err != nil {
		return Status{}, fmt.Errorf("decode update status: %w", err)
	}

	if status.SchemaVersion != SchemaVersion {
		return Status{}, fmt.Errorf("unsupported update status schemaVersion %q", status.SchemaVersion)
	}

	return status, nil
}

// Answers reports whether the status is the agent's answer to the request: for
// the same update and operation. A status left over from an earlier request
// doesn't answer a new one. The commit status, written after the restart,
// carries the operation ID of the finalize request it follows.
func (s Status) Answers(r Request) bool {
	return s.NodeUpdateID == r.NodeUpdateID && s.OperationID == r.OperationID
}
