// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Write request kinds on the client socket (designs/racer-writes.md, Client API).
const (
	WritePut       = "put"
	WriteHostWrite = "host-write"
	WritePersist   = "persist"
	WriteDelete    = "delete"
)

// MaxObjectPages is the Azure Blob block limit that bounds a written object.
const MaxObjectPages = 50_000

// MaxObjectBytes is the largest object a client may write with the default
// page size.
const MaxObjectBytes = MaxObjectPages * PageSize

// MaxHoldSeconds is MAX_HOLD_TTL (30 days).
const MaxHoldSeconds = 2_592_000

// Upload modes and states.
const (
	UploadAuto   = "auto"
	UploadManual = "manual"
	UploadNever  = "never"

	StatePending   = "pending"
	StateUploading = "uploading"
	StateCommitted = "committed"
	StateFailed    = "failed"
	StateNever     = "never"
	StateManual    = "manual"
)

// ValidUploadMode reports whether s is a Racer-Upload value.
func ValidUploadMode(s string) bool {
	return s == UploadAuto || s == UploadManual || s == UploadNever
}

// ValidUploadState reports whether s is a Racer-Upload-State value.
func ValidUploadState(s string) bool {
	switch s {
	case StatePending, StateUploading, StateCommitted, StateFailed, StateNever, StateManual:
		return true
	default:
		return false
	}
}

// WriteOptions are the per-request write headers. Zero values select the
// cache default.
type WriteOptions struct {
	Replicas      int
	OriginDurable bool
	Upload        string
	HoldSeconds   uint64
	ContentType   string
	// Length is Content-Length for put and Racer-Source-Length for host-write.
	Length uint64
	// SourceOffset is Racer-Source-Offset for host-write.
	SourceOffset uint64
}

// HasVersionID reports whether opaque metadata is a JSON object naming a
// version, which writes must not carry.
func HasVersionID(metadata string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(metadata), &fields) != nil {
		return false
	}

	_, ok := fields["versionId"]

	return ok
}

// WriteHead builds the request head of a write operation. kind is one of
// WritePut, WriteHostWrite, WritePersist, WriteDelete.
func WriteHead(kind string, r Request, o WriteOptions) ([]byte, error) {
	bad := func(what string) error {
		return failure(ErrorInvalidArgument, "write", fmt.Errorf("%s", what))
	}

	if r.AdapterMetadata != "" && HasVersionID(r.AdapterMetadata) {
		return nil, bad("writes must not name a versionId")
	}

	key := hex.EncodeToString(r.Key[:])

	var b strings.Builder

	switch kind {
	case WritePut:
		if o.Length == 0 || o.Length > MaxObjectBytes {
			return nil, bad("invalid object length")
		}

		fmt.Fprintf(&b, "PUT %s%s HTTP/1.1\r\nHost: racer\r\nContent-Length: %d\r\nIf-None-Match: *\r\n", ObjectPrefix, key, o.Length)
	case WriteHostWrite:
		if o.Length == 0 || o.Length > MaxObjectBytes || o.SourceOffset > uint64(1<<63-1)-o.Length {
			return nil, bad("invalid source window")
		}

		fmt.Fprintf(&b, "POST %s%s/host-write HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\nIf-None-Match: *\r\n"+
			"Racer-Source-Offset: %d\r\nRacer-Source-Length: %d\r\n", ObjectPrefix, key, o.SourceOffset, o.Length)
	case WritePersist:
		fmt.Fprintf(&b, "POST %s%s/persist HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n", ObjectPrefix, key)
	case WriteDelete:
		fmt.Fprintf(&b, "DELETE %s%s HTTP/1.1\r\nHost: racer\r\nContent-Length: 0\r\n", ObjectPrefix, key)
	default:
		return nil, bad("unknown write kind")
	}

	if kind == WritePut || kind == WriteHostWrite {
		if o.Replicas < 0 || o.Replicas > 3 || o.OriginDurable && o.Replicas != 0 {
			return nil, bad("invalid durability")
		}

		if o.Upload != "" && !ValidUploadMode(o.Upload) {
			return nil, bad("invalid upload mode")
		}

		if o.OriginDurable && (o.Upload == UploadManual || o.Upload == UploadNever) {
			return nil, bad("origin durability needs auto upload")
		}

		if o.HoldSeconds > MaxHoldSeconds {
			return nil, bad("invalid hold")
		}

		switch {
		case o.OriginDurable:
			b.WriteString("Racer-Durability: origin\r\n")
		case o.Replicas != 0:
			b.WriteString("Racer-Durability: replicas=" + strconv.Itoa(o.Replicas) + "\r\n")
		}

		if o.Upload != "" {
			b.WriteString("Racer-Upload: " + o.Upload + "\r\n")
		}

		if o.HoldSeconds != 0 {
			b.WriteString("Racer-Hold-Seconds: " + strconv.FormatUint(o.HoldSeconds, 10) + "\r\n")
		}

		if o.ContentType != "" {
			if ValidateContentType(o.ContentType) != nil {
				return nil, bad("invalid content type")
			}

			b.WriteString("Racer-Content-Type: " + o.ContentType + "\r\n")
		}
	}

	if r.AdapterMetadata != "" {
		b.WriteString("Racer-Metadata: " + r.AdapterMetadata + "\r\n")
	}

	if r.Authorization != "" {
		b.WriteString("Authorization: " + r.Authorization + "\r\n")
	}

	b.WriteString("\r\n")
	head := []byte(b.String())

	return head, ValidateRawHead(head, false)
}

// WriteResponse is a parsed write response.
type WriteResponse struct {
	Status      int
	ETag        string
	UploadState string
	// Error is the Racer-Write-Error diagnostic token of a failure.
	Error string
	// RetryAfter is the Retry-After value in seconds, or -1 when absent.
	RetryAfter int64
}

// ParseWriteResponse parses a write response head. Success is 201 for put
// and host-write and 204 for persist and delete; other statuses return a
// WriteResponse with Status set and a nil error so the caller can classify
// them. Protocol violations return an error.
func ParseWriteResponse(head []byte, kind string) (WriteResponse, error) {
	bad := failure(ErrorProtocol, "write response", nil)
	out := WriteResponse{RetryAfter: -1}

	if err := ValidateRawHead(head, true); err != nil {
		return out, err
	}

	line := head[:strings.Index(string(head), "\r\n")]
	if len(line) < 12 || string(line[:9]) != "HTTP/1.1 " || len(line) > 12 && line[12] != ' ' {
		return out, bad
	}

	for _, c := range line[9:12] {
		if c < '0' || c > '9' {
			return out, bad
		}

		out.Status = out.Status*10 + int(c-'0')
	}

	h := HeadHeaders(head)
	if h.Get("Transfer-Encoding") != "" {
		return out, bad
	}

	// Write responses never carry a body. 204 may omit Content-Length.
	if _, present := h["Content-Length"]; present || out.Status != 204 {
		if length, err := Decimal(h.Get("Content-Length")); err != nil || length != 0 {
			return out, bad
		}
	}

	out.Error = h.Get("Racer-Write-Error")

	if v := h.Get("Retry-After"); v != "" {
		n, err := Decimal(v)
		if err == nil {
			out.RetryAfter = int64(n)
		}
	}

	state, hasState := h["Racer-Upload-State"]
	if hasState {
		if !ValidUploadState(state[0]) {
			return out, bad
		}

		out.UploadState = state[0]
	}

	switch out.Status {
	case 201:
		if kind != WritePut && kind != WriteHostWrite {
			return out, bad
		}

		out.ETag = h.Get("Etag")
		if ValidateETag(out.ETag) != nil || !hasState {
			return out, bad
		}
	case 204:
		if kind == WritePut || kind == WriteHostWrite || kind == WritePersist && !hasState {
			return out, bad
		}
	}

	return out, nil
}
