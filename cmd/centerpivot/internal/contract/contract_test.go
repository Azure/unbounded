// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package contract

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var source = Source{
	Server: "https://nebraska.example.com/v1/update",
	AppID:  "11111111-2222-3333-4444-555555555555",
	Track:  "stable",
}

func TestKeysFor(t *testing.T) {
	t.Parallel()

	assert.Equal(t, Keys{
		Request:      "centerpivot.unbounded-cloud.io/update-request",
		Status:       "centerpivot.unbounded-cloud.io/update-status",
		CommitStatus: "centerpivot.unbounded-cloud.io/update-commit-status",
	}, KeysFor("centerpivot.unbounded-cloud.io"))
}

func TestNewRequest(t *testing.T) {
	t.Parallel()

	stage := NewRequest(OperationStage, "", "202606.29.0", source)
	require.NoError(t, stage.Validate())

	finalize := NewRequest(OperationFinalize, stage.NodeUpdateID, "202606.29.0", source)
	require.NoError(t, finalize.Validate())

	assert.Equal(t, stage.NodeUpdateID, finalize.NodeUpdateID, "a finalize continues the update its stage started")
	assert.NotEqual(t, stage.OperationID, finalize.OperationID, "each request has its own operation ID")
	assert.NotEqual(t, stage.NodeUpdateID, NewRequest(OperationStage, "", "202606.29.0", source).NodeUpdateID,
		"each stage starts a new update")
}

func TestMarshalMatchesTheContract(t *testing.T) {
	t.Parallel()

	request := Request{
		SchemaVersion: SchemaVersion,
		NodeUpdateID:  "550e8400-e29b-41d4-a716-446655440000",
		OperationID:   "c9d6f0a2-3b41-4e8d-9f27-1a5b6c7d8e90",
		Operation:     OperationStage,
		TargetVersion: "202606.29.0",
		Server:        source.Server,
		AppID:         source.AppID,
		Track:         "pin-202606.29.0",
	}

	value, err := request.Marshal()
	require.NoError(t, err)

	// The request annotation in Trident's documentation, field for field.
	assert.JSONEq(t, `{
		"schemaVersion": "1.0",
		"nodeUpdateId":  "550e8400-e29b-41d4-a716-446655440000",
		"operationId":   "c9d6f0a2-3b41-4e8d-9f27-1a5b6c7d8e90",
		"operation":     "stage",
		"targetVersion": "202606.29.0",
		"server":        "https://nebraska.example.com/v1/update",
		"appId":         "11111111-2222-3333-4444-555555555555",
		"track":         "pin-202606.29.0"
	}`, value)
}

func TestValidateRefusesWhatTheAgentRefuses(t *testing.T) {
	t.Parallel()

	valid := NewRequest(OperationStage, "", "202606.29.0", source)

	for name, change := range map[string]func(*Request){
		"another schema version":            func(r *Request) { r.SchemaVersion = "2.0" },
		"an update ID that isn't a UUID":    func(r *Request) { r.NodeUpdateID = "update-1" },
		"an operation ID that isn't a UUID": func(r *Request) { r.OperationID = "op-1" },
		"a rollback":                        func(r *Request) { r.Operation = OperationRollback },
		"no target version":                 func(r *Request) { r.TargetVersion = "" },
		"an http server":                    func(r *Request) { r.Server = "http://nebraska.example.com/v1/update" },
		"no app ID":                         func(r *Request) { r.AppID = "" },
		"no track":                          func(r *Request) { r.Track = "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			request := valid
			change(&request)

			_, err := request.Marshal()
			assert.Error(t, err)
		})
	}
}

func TestParseStatus(t *testing.T) {
	t.Parallel()

	// The status annotation in Trident's documentation, with a field the
	// contract doesn't have yet.
	status, err := ParseStatus(`{
		"schemaVersion":   "1.0",
		"nodeUpdateId":    "550e8400-e29b-41d4-a716-446655440000",
		"operationId":     "c9d6f0a2-3b41-4e8d-9f27-1a5b6c7d8e90",
		"operation":       "stage",
		"code":            "Success",
		"message":         "staged update to 202606.29.0",
		"fromVersion":     "202606.15.0",
		"toVersion":       "202606.29.0",
		"tridentVersion":  "0.22.0",
		"startedUtc":      "2026-06-29T12:00:00Z",
		"lastUpdatedUtc":  "2026-06-29T12:03:41Z",
		"finishedUtc":     "2026-06-29T12:03:41Z",
		"addedLater":      true
	}`)
	require.NoError(t, err)

	assert.Equal(t, OperationStage, status.Operation)
	assert.Equal(t, CodeSuccess, status.Code)
	assert.True(t, status.Code.Final())
	assert.Equal(t, "202606.29.0", status.ToVersion)
	require.NotNil(t, status.FinishedUTC)

	_, err = ParseStatus(`{"schemaVersion": "2.0"}`)
	assert.Error(t, err)

	_, err = ParseStatus(`not json`)
	assert.Error(t, err)
}

func TestAnswers(t *testing.T) {
	t.Parallel()

	finalize := NewRequest(OperationFinalize, uuid.NewString(), "202606.29.0", source)
	commit := Status{NodeUpdateID: finalize.NodeUpdateID, OperationID: finalize.OperationID, Operation: OperationCommit}

	assert.True(t, commit.Answers(finalize))

	earlier := Status{NodeUpdateID: finalize.NodeUpdateID, OperationID: uuid.NewString()}
	assert.False(t, earlier.Answers(finalize), "a status for an earlier request")
	assert.False(t, CodeInProgress.Final())
}
