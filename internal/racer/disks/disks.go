// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Package disks holds the Racer disk-consent annotation contract shared by
// the dataplane and kubectl-unbounded: the Node annotation that approves raw
// block devices and the Pod annotation that reports what the dataplane found.
package disks

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

const (
	// Annotation is the Node annotation listing approved /dev/disk/by-id
	// basenames, comma separated. Approving a disk consents to Racer wiping
	// its first and last 1 MiB.
	Annotation = "racer.unbounded-cloud.io/disks"

	// RevisionAnnotation is the Node annotation holding an opaque token that
	// changes with every Annotation change made by kubectl-unbounded. The
	// dataplane echoes it in Report.DisksRevision once the disk set it
	// applied matches Annotation.
	RevisionAnnotation = "racer.unbounded-cloud.io/disks-revision"

	// ReportAnnotation is the dataplane Pod annotation holding the JSON report.
	ReportAnnotation = "racer.unbounded-cloud.io/report"

	// LegacyBlockDevicesAnnotation is the former regex consent annotation. It
	// is no longer honored.
	LegacyBlockDevicesAnnotation = "racer.unbounded-cloud.io/block-devices"

	// MaxAnnotationBytes bounds the Annotation value.
	MaxAnnotationBytes = 4096

	// MaxIDBytes bounds a single disk ID, matching the dataplane.
	MaxIDBytes = 255

	// MaxDisks bounds the number of approved disks, matching the dataplane.
	MaxDisks = 256
)

// Disk states reported by the dataplane.
const (
	StateClaimed     = "claimed"
	StateAvailable   = "available"
	StateUnavailable = "unavailable"
	StateRejected    = "rejected"
	StateMissing     = "missing"
)

// ErrInvalidID reports a malformed disk ID.
var ErrInvalidID = errors.New("invalid disk id")

// Disk is one entry of the report's disks list.
type Disk struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases,omitempty"`
	Dev       string   `json:"dev,omitempty"`
	SizeBytes uint64   `json:"size_bytes,omitempty"`
	Model     string   `json:"model,omitempty"`
	Serial    string   `json:"serial,omitempty"`
	State     string   `json:"state"`
	Reason    string   `json:"reason,omitempty"`
}

// Report is the subset of the dataplane report the CLI consumes. Disks is nil
// when the dataplane predates disk reporting.
type Report struct {
	Disks []Disk `json:"disks"`

	// DisksRevision is the RevisionAnnotation value whose approved set the
	// reported disk states reflect. Empty when unknown.
	DisksRevision string `json:"disks_revision,omitempty"`
}

// NewRevision returns a fresh random RevisionAnnotation value.
func NewRevision() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate disk approval revision: %w", err)
	}

	return hex.EncodeToString(b), nil
}

// ValidRevision reports whether value is a RevisionAnnotation the dataplane
// accepts and echoes: 1 to 64 ASCII letters, digits, '.', '_', or '-'.
func ValidRevision(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}

	for _, r := range value {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-'
		if !ok {
			return false
		}
	}

	return true
}

// ParseReport decodes a report annotation value. Unknown fields are ignored.
func ParseReport(value string) (*Report, error) {
	var r Report
	if err := json.Unmarshal([]byte(value), &r); err != nil {
		return nil, fmt.Errorf("parse racer report: %w", err)
	}

	for i, d := range r.Disks {
		if d.ID == "" {
			return nil, fmt.Errorf("parse racer report: disk %d has no id", i)
		}
	}

	return &r, nil
}

// Find returns the disk whose id or any alias equals name.
func (r *Report) Find(name string) (Disk, bool) {
	if r == nil {
		return Disk{}, false
	}

	for _, d := range r.Disks {
		if d.ID == name || slices.Contains(d.Aliases, name) {
			return d, true
		}
	}

	return Disk{}, false
}

// ValidateID checks a single disk ID using the same rules as the dataplane:
// 1 to MaxIDBytes printable non-space ASCII bytes, without '/' or ',', and
// not "." or "..".
func ValidateID(id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: empty", ErrInvalidID)
	case len(id) > MaxIDBytes:
		return fmt.Errorf("%w %q: longer than %d bytes", ErrInvalidID, id, MaxIDBytes)
	case id == "." || id == "..":
		return fmt.Errorf("%w %q", ErrInvalidID, id)
	}

	for i := range len(id) {
		if b := id[i]; b <= ' ' || b > '~' || b == '/' || b == ',' {
			return fmt.Errorf("%w %q: must be printable ASCII without '/', ',' or whitespace", ErrInvalidID, id)
		}
	}

	return nil
}

// ParseAnnotation splits an Annotation value into its trimmed, non-empty
// entries. Invalid entries are returned too; callers decide how to treat them.
func ParseAnnotation(value string) []string {
	var out []string

	for e := range strings.SplitSeq(value, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}

	return out
}

// ValidateAnnotation reports whether the dataplane would accept value. Like
// the dataplane, it rejects values over MaxAnnotationBytes, any invalid entry,
// or more than MaxDisks distinct entries; a rejected value approves no disks.
func ValidateAnnotation(value string) error {
	if len(value) > MaxAnnotationBytes {
		return fmt.Errorf("annotation %s is %d bytes, max %d", Annotation, len(value), MaxAnnotationBytes)
	}

	ids := ParseAnnotation(value)
	for _, id := range ids {
		if err := ValidateID(id); err != nil {
			return err
		}
	}

	slices.Sort(ids)

	if n := len(slices.Compact(ids)); n > MaxDisks {
		return fmt.Errorf("annotation %s lists %d disks, max %d", Annotation, n, MaxDisks)
	}

	return nil
}

// FormatAnnotation dedupes, sorts, and joins ids. It fails if any id is
// invalid or the result exceeds MaxAnnotationBytes.
func FormatAnnotation(ids []string) (string, error) {
	s := slices.Clone(ids)
	slices.Sort(s)
	s = slices.Compact(s)

	for _, id := range s {
		if err := ValidateID(id); err != nil {
			return "", err
		}
	}

	if len(s) > MaxDisks {
		return "", fmt.Errorf("annotation %s would list %d disks, max %d", Annotation, len(s), MaxDisks)
	}

	v := strings.Join(s, ",")
	if len(v) > MaxAnnotationBytes {
		return "", fmt.Errorf("annotation %s would be %d bytes, max %d", Annotation, len(v), MaxAnnotationBytes)
	}

	return v, nil
}
