// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package contract defines the wire types exchanged between the Unbounded
// management dashboard and the component modules it renders.
//
// A module is any Unbounded component (net, storage, gantry, ...) that exposes
// the small HTTP surface described here. The dashboard server discovers modules
// from a static registry, fetches their structured data, and renders it through
// shared server-side templates. Modules never return presentation markup; they
// return the structured types in this package so the dashboard can render a
// consistent UI and so the same data is available as JSON for automation.
//
// The contract is intentionally minimal for the prototype. It favours a small
// number of generic, composable primitives (cards, metrics, tables, detail
// lists, action forms) over component-specific schemas.
package contract

// Capability is a named feature a module advertises in its manifest. The
// dashboard uses capabilities to decide which navigation entries and pages to
// expose for a module.
type Capability string

const (
	// CapabilitySummary indicates the module serves a Summary surface.
	CapabilitySummary Capability = "summary"
	// CapabilityResources indicates the module serves Resource collections.
	CapabilityResources Capability = "resources"
	// CapabilityDetails indicates the module serves per-resource Detail views.
	CapabilityDetails Capability = "details"
	// CapabilityActions indicates the module exposes invokable Actions.
	CapabilityActions Capability = "actions"
	// CapabilityStream indicates the module exposes a live update stream.
	CapabilityStream Capability = "stream"
)

// Health is a coarse status classification shared by summaries, badges, and
// alerts so the dashboard can apply consistent colours across modules.
type Health string

const (
	// HealthUnknown means the module could not determine a status.
	HealthUnknown Health = "unknown"
	// HealthOK means the surface is healthy.
	HealthOK Health = "ok"
	// HealthWarning means the surface needs attention but is functional.
	HealthWarning Health = "warning"
	// HealthError means the surface is in a failed or degraded state.
	HealthError Health = "error"
)

// Permission describes a Kubernetes RBAC check the dashboard should perform
// (via SubjectAccessReview) before exposing a module surface or action.
type Permission struct {
	APIGroup string `json:"apiGroup"`
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
	Name     string `json:"name,omitempty"`
}

// Manifest is the self-description a module returns from its manifest endpoint.
// It tells the dashboard how to label the module, which surfaces it supports,
// and what permissions are required to view it.
type Manifest struct {
	ID                  string       `json:"id"`
	Title               string       `json:"title"`
	Description         string       `json:"description,omitempty"`
	Capabilities        []Capability `json:"capabilities"`
	RequiredPermissions []Permission `json:"requiredPermissions,omitempty"`
	// ResourceKinds lists the resource collection kinds the module serves.
	// Each kind maps to /resources/{kind} on the module and a navigation entry
	// in the dashboard.
	ResourceKinds []ResourceKind `json:"resourceKinds,omitempty"`
}

// HasCapability reports whether the manifest advertises the given capability.
func (m Manifest) HasCapability(c Capability) bool {
	for _, have := range m.Capabilities {
		if have == c {
			return true
		}
	}

	return false
}

// ResourceKind describes a collection of like resources a module owns.
type ResourceKind struct {
	// Kind is the URL-safe identifier used in /resources/{kind}.
	Kind string `json:"kind"`
	// Title is the human-readable plural label, e.g. "Widgets".
	Title string `json:"title"`
	// Singular is the human-readable singular label, e.g. "Widget".
	Singular string `json:"singular,omitempty"`
}

// Summary is the module overview surface: an overall health roll-up, key
// metrics, and any active alerts.
type Summary struct {
	Health  Health   `json:"health"`
	Message string   `json:"message,omitempty"`
	Metrics []Metric `json:"metrics,omitempty"`
	Alerts  []Alert  `json:"alerts,omitempty"`
}

// Metric is a single labelled value rendered as a metric card.
type Metric struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Unit   string `json:"unit,omitempty"`
	Health Health `json:"health,omitempty"`
}

// Alert is a warning or error surfaced to the operator.
type Alert struct {
	Health Health `json:"health"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	Source string `json:"source,omitempty"`
}

// ResourceList is a table of resources of a single kind.
type ResourceList struct {
	Kind    string        `json:"kind"`
	Title   string        `json:"title"`
	Columns []Column      `json:"columns"`
	Rows    []ResourceRow `json:"rows"`
}

// Column is a table column definition.
type Column struct {
	// Key matches a key in ResourceRow.Cells.
	Key   string `json:"key"`
	Title string `json:"title"`
}

// ResourceRow is one row in a ResourceList. Name identifies the resource for
// detail lookups; Cells holds the rendered column values keyed by Column.Key.
type ResourceRow struct {
	Name   string            `json:"name"`
	Health Health            `json:"health,omitempty"`
	Cells  map[string]string `json:"cells"`
}

// ResourceDetail is the per-resource view: a set of detail sections plus any
// actions applicable to this resource.
type ResourceDetail struct {
	Kind     string          `json:"kind"`
	Name     string          `json:"name"`
	Title    string          `json:"title"`
	Health   Health          `json:"health,omitempty"`
	Sections []DetailSection `json:"sections,omitempty"`
	Actions  []ActionRef     `json:"actions,omitempty"`
}

// DetailSection is a titled list of key/value fields.
type DetailSection struct {
	Title  string        `json:"title"`
	Fields []DetailField `json:"fields"`
}

// DetailField is a single labelled value in a DetailSection.
type DetailField struct {
	Label  string `json:"label"`
	Value  string `json:"value"`
	Health Health `json:"health,omitempty"`
}

// ActionRef names an action that can be invoked, with the form fields the
// dashboard should render to collect its parameters.
type ActionRef struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Permission, when set, is checked via SAR before the action is offered or
	// executed.
	Permission *Permission   `json:"permission,omitempty"`
	Fields     []ActionField `json:"fields,omitempty"`
}

// ActionField describes a single input in an action form.
type ActionField struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Type     string `json:"type,omitempty"` // text, number, select, checkbox
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	// Options populates a select field.
	Options []string `json:"options,omitempty"`
}

// ActionResult is the module's response to an invoked action.
type ActionResult struct {
	Health  Health `json:"health"`
	Message string `json:"message"`
}

// StreamEvent is one server-sent event on a module's live stream. Surface
// names the surface that changed (e.g. "summary") so the dashboard knows which
// region to refresh.
type StreamEvent struct {
	Surface string   `json:"surface"`
	Summary *Summary `json:"summary,omitempty"`
}
