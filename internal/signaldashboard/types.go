// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package signaldashboard

import "time"

type APIError struct {
	Resource string `json:"resource"`
	Message  string `json:"message"`
}

type HealthStatus struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type Summary struct {
	Counts      RunCounts         `json:"counts"`
	NodeCounts  map[string]int    `json:"nodeCounts"`
	Controller  HealthStatus      `json:"controller"`
	Argo        HealthStatus      `json:"argo"`
	Reasons     map[string]string `json:"reasonExplanations"`
	LastRefresh time.Time         `json:"lastRefresh"`
	Partial     bool              `json:"partial"`
	Errors      []APIError        `json:"errors,omitempty"`
}

type RunCounts struct {
	Running      int `json:"running"`
	Pending      int `json:"pending"`
	Passed       int `json:"passed"`
	Failed       int `json:"failed"`
	Inconclusive int `json:"inconclusive"`
}

type RunList struct {
	Items       []RunSummary `json:"items"`
	Total       int          `json:"total"`
	Offset      int          `json:"offset"`
	Limit       int          `json:"limit"`
	LastRefresh time.Time    `json:"lastRefresh"`
	Partial     bool         `json:"partial"`
	Errors      []APIError   `json:"errors,omitempty"`
}

type RunSummary struct {
	Name          string     `json:"name"`
	Profile       string     `json:"profile"`
	HardwareClass string     `json:"hardwareClass"`
	NodeAliases   []string   `json:"nodeAliases"`
	Phase         string     `json:"phase"`
	Execution     string     `json:"execution"`
	Verdict       string     `json:"verdict"`
	Reason        string     `json:"reason"`
	Explanation   string     `json:"explanation"`
	OutcomeType   string     `json:"outcomeType"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Duration      string     `json:"duration"`
	NextDueAt     *time.Time `json:"nextDueAt,omitempty"`
	WorkflowName  string     `json:"workflowName,omitempty"`
	WorkflowPhase string     `json:"workflowPhase,omitempty"`
	MPIJobName    string     `json:"mpiJobName,omitempty"`
	MPIJobPhase   string     `json:"mpiJobPhase,omitempty"`
	ScriptDigest  string     `json:"scriptDigest,omitempty"`
	Trigger       string     `json:"trigger"`
	Attempt       int32      `json:"attempt"`
}

type RunDetail struct {
	RunSummary
	Stages        []StageStatus       `json:"stages"`
	WorkflowNodes []WorkflowNode      `json:"workflowNodes"`
	Outputs       map[string]string   `json:"outputs,omitempty"`
	MPIJob        *MPIJobStatus       `json:"mpiJob,omitempty"`
	Participants  []ParticipantStatus `json:"participants"`
	Cleanup       CleanupStatus       `json:"cleanup"`
	Partial       bool                `json:"partial"`
	Errors        []APIError          `json:"errors,omitempty"`
}

type StageStatus struct {
	Name       string     `json:"name"`
	Phase      string     `json:"phase"`
	Reason     string     `json:"reason,omitempty"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	Source     string     `json:"source"`
}

type WorkflowNode struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DisplayName string     `json:"displayName"`
	Template    string     `json:"template"`
	Type        string     `json:"type"`
	Phase       string     `json:"phase"`
	Reason      string     `json:"reason,omitempty"`
	Message     string     `json:"message,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
}

type MPIJobStatus struct {
	Name            string                  `json:"name"`
	Phase           string                  `json:"phase"`
	Conditions      []MPIJobCondition       `json:"conditions"`
	ReplicaStatuses map[string]ReplicaState `json:"replicaStatuses,omitempty"`
}

type MPIJobCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

type ReplicaState struct {
	Active    int64 `json:"active"`
	Succeeded int64 `json:"succeeded"`
	Failed    int64 `json:"failed"`
}

type ParticipantStatus struct {
	Alias       string `json:"alias"`
	State       string `json:"state"`
	Eligibility string `json:"eligibility"`
	Reserved    bool   `json:"reserved"`
}

type CleanupStatus struct {
	ReservationsReleased bool `json:"reservationsReleased"`
	SnapshotDeleted      bool `json:"snapshotDeleted"`
}

type NodeList struct {
	Items       []NodeSummary `json:"items"`
	LastRefresh time.Time     `json:"lastRefresh"`
	Partial     bool          `json:"partial"`
	Errors      []APIError    `json:"errors,omitempty"`
}

type NodeSummary struct {
	Alias              string     `json:"alias"`
	HardwareClass      string     `json:"hardwareClass"`
	Profile            string     `json:"profile"`
	State              string     `json:"state"`
	Eligibility        string     `json:"eligibility"`
	ActiveRun          string     `json:"activeRun,omitempty"`
	LastVerdict        string     `json:"lastVerdict,omitempty"`
	LastReason         string     `json:"lastReason,omitempty"`
	LastRunAt          *time.Time `json:"lastRunAt,omitempty"`
	NextDueAt          *time.Time `json:"nextDueAt,omitempty"`
	RecurringSuspended bool       `json:"recurringSuspended"`
	RecurringReason    string     `json:"recurringReason,omitempty"`
	Explanation        string     `json:"explanation,omitempty"`
}
