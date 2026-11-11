// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package fakeracer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// Written is an object created through the fake write API.
type Written struct {
	Data          []byte
	ETag          string
	ContentType   string
	Metadata      string
	Replicas      int
	OriginDurable bool
	Upload        string
	HoldSeconds   uint64
	State         string
	// Created is when the write was acknowledged.
	Created time.Time
}

// WriteCall describes one write request the fake received.
type WriteCall struct {
	// Kind is put, host-write, persist, or delete.
	Kind   string
	Key    [32]byte
	Header http.Header
	// Files is the number of descriptors received with the head.
	Files int
}

// WriteReply is an injected failure.
type WriteReply struct {
	Status int
	// Code is sent as Racer-Write-Error when nonempty.
	Code string
	// RetryAfter is sent as Retry-After when positive.
	RetryAfter int
}

// Writes is the fake's write directory. Its zero value is not usable; use
// NewWrites. Methods are safe for concurrent use.
type Writes struct {
	mu      sync.Mutex
	objects map[[32]byte]*Written
	calls   []WriteCall
	enabled bool
	reject  func(WriteCall) *WriteReply
	// defaults
	upload string
	// originExists reports whether the origin already has the object. A
	// non-nil error means the origin could not answer; the create fails.
	originExists func(ctx context.Context, key [32]byte, metadata, authorization string) (bool, error)
}

// NewWrites returns an enabled write directory whose default upload mode is
// auto.
func NewWrites() *Writes {
	return &Writes{objects: make(map[[32]byte]*Written), enabled: true, upload: wire.UploadAuto}
}

// SetEnabled turns writes on or off. Disabled writes answer 405 like a cache
// without writes.
func (w *Writes) SetEnabled(enabled bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.enabled = enabled
}

// SetDefaultUpload sets the upload mode used when a request names none.
func (w *Writes) SetDefaultUpload(mode string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.upload = mode
}

// SetReject installs a hook that runs after header validation. A non-nil
// reply is sent instead of performing the write.
func (w *Writes) SetReject(reject func(WriteCall) *WriteReply) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.reject = reject
}

// SetOriginExists installs the create-once origin check. When the check
// returns an error the create fails with 503 and creates nothing, as Racer
// does when its origin probe fails.
func (w *Writes) SetOriginExists(exists func(ctx context.Context, key [32]byte, metadata, authorization string) (bool, error)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.originExists = exists
}

// Object returns a copy of the written object for key.
func (w *Writes) Object(key [32]byte) (Written, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()

	o, ok := w.objects[key]
	if !ok {
		return Written{}, false
	}

	return *o, true
}

// SetState changes the upload state of a written object, for example to
// simulate a completed upload.
func (w *Writes) SetState(key [32]byte, state string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	o, ok := w.objects[key]
	if ok {
		o.State = state
	}

	return ok
}

// Calls returns the write requests received so far.
func (w *Writes) Calls() []WriteCall {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]WriteCall(nil), w.calls...)
}

// IsWrite reports whether r is a write request: PUT or DELETE on an object,
// or POST on its host-write or persist path.
func IsWrite(r *http.Request) bool {
	_, kind, ok := writeTarget(r)
	return ok && kind != ""
}

func writeTarget(r *http.Request) ([32]byte, string, bool) {
	rest, ok := strings.CutPrefix(r.URL.Path, wire.ObjectPrefix)
	if !ok {
		return [32]byte{}, "", false
	}

	hexKey, suffix, _ := strings.Cut(rest, "/")

	key, err := wire.ParseKey(hexKey)
	if err != nil {
		return [32]byte{}, "", false
	}

	switch {
	case suffix == "" && r.Method == http.MethodPut:
		return key, wire.WritePut, true
	case suffix == "" && r.Method == http.MethodDelete:
		return key, wire.WriteDelete, true
	case suffix == "host-write" && r.Method == http.MethodPost:
		return key, wire.WriteHostWrite, true
	case suffix == "persist" && r.Method == http.MethodPost:
		return key, wire.WritePersist, true
	default:
		return key, "", true
	}
}

// ServeHead answers a HEAD for a written object and reports whether it did.
func (w *Writes) ServeHead(rw http.ResponseWriter, r *http.Request) bool {
	key, _, ok := writeTarget(r)
	if !ok || r.Method != http.MethodHead {
		return false
	}

	o, ok := w.Object(key)
	if !ok {
		return false
	}

	if pin := r.Header.Get("If-Match"); pin != "" && pin != o.ETag {
		writeError(rw, 412)
		return true
	}

	h := rw.Header()
	h.Set("Etag", o.ETag)
	h.Set("Racer-Expires-At", strconv.FormatInt(o.Created.Add(time.Duration(o.HoldSeconds)*time.Second).UnixMilli(), 10))
	h.Set("Racer-Upload-State", o.State)
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.Itoa(len(o.Data)))

	if o.ContentType != "" {
		h.Set("Racer-Content-Type", o.ContentType)
	}

	rw.WriteHeader(200)

	return true
}

func writeFailure(rw http.ResponseWriter, reply WriteReply) {
	h := rw.Header()
	h.Set("Content-Length", "0")

	if reply.Status == 405 {
		h.Set("Allow", "HEAD, POST")
	}

	if reply.Code != "" {
		h.Set("Racer-Write-Error", reply.Code)
	}

	if reply.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(reply.RetryAfter))
	}

	rw.WriteHeader(reply.Status)
}

// ServeWrite handles a write request. files are the descriptors received
// with the head; ServeWrite does not close them.
func (w *Writes) ServeWrite(rw http.ResponseWriter, r *http.Request, files []*os.File) {
	key, kind, _ := writeTarget(r)
	call := WriteCall{Kind: kind, Key: key, Header: r.Header.Clone(), Files: len(files)}

	w.mu.Lock()
	w.calls = append(w.calls, call)
	enabled, reject, defaultUpload, originExists := w.enabled, w.reject, w.upload, w.originExists
	w.mu.Unlock()

	if !enabled {
		writeFailure(rw, WriteReply{Status: 405})
		return
	}

	h := r.Header
	metadata, authorization := h.Get("Racer-Metadata"), h.Get("Authorization")

	if r.Host != "racer" || metadata != "" && wire.HasVersionID(metadata) {
		writeFailure(rw, WriteReply{Status: 400})
		return
	}

	var spec Written

	if kind == wire.WritePut || kind == wire.WriteHostWrite {
		var status int

		spec, status = parseSpec(h, defaultUpload)
		if status == 0 && kind == wire.WritePut {
			status = putLength(r)
		}

		if status == 0 && kind == wire.WriteHostWrite && (r.ContentLength != 0 || len(files) != 1) {
			status = 400
		}

		if status != 0 {
			writeFailure(rw, WriteReply{Status: status})
			return
		}
	} else if r.ContentLength > 0 {
		writeFailure(rw, WriteReply{Status: 400})
		return
	}

	if reject != nil {
		if reply := reject(call); reply != nil {
			writeFailure(rw, *reply)
			return
		}
	}

	switch kind {
	case wire.WritePut, wire.WriteHostWrite:
		w.create(rw, r, key, kind, spec, files, originExists, metadata, authorization)
	case wire.WritePersist:
		w.persist(rw, key)
	case wire.WriteDelete:
		w.remove(rw, key)
	}
}

func putLength(r *http.Request) int {
	switch {
	case r.ContentLength < 0:
		return 411
	case r.ContentLength == 0:
		return 400
	case r.ContentLength > wire.MaxObjectBytes:
		return 413
	default:
		return 0
	}
}

func parseSpec(h http.Header, defaultUpload string) (Written, int) {
	spec := Written{Upload: defaultUpload, Replicas: 2, HoldSeconds: 86400, ContentType: h.Get("Racer-Content-Type")}

	if v := h.Values("If-None-Match"); len(v) != 1 || v[0] != "*" {
		return spec, 428
	}

	if v := h.Get("Racer-Upload"); v != "" {
		if !wire.ValidUploadMode(v) {
			return spec, 400
		}

		spec.Upload = v
	}

	switch v := h.Get("Racer-Durability"); v {
	case "":
	case "origin":
		spec.OriginDurable, spec.Replicas = true, 0
	case "replicas=1", "replicas=2", "replicas=3":
		spec.Replicas = int(v[len(v)-1] - '0')
	default:
		return spec, 400
	}

	if spec.OriginDurable && spec.Upload != wire.UploadAuto {
		return spec, 400
	}

	if v := h.Get("Racer-Hold-Seconds"); v != "" {
		n, err := wire.Decimal(v)
		if err != nil || n == 0 || n > wire.MaxHoldSeconds {
			return spec, 400
		}

		spec.HoldSeconds = n
	}

	if spec.Upload != wire.UploadNever && h.Get("Racer-Metadata") == "" {
		return spec, 400
	}

	return spec, 0
}

func (w *Writes) create(rw http.ResponseWriter, r *http.Request, key [32]byte, kind string, spec Written,
	files []*os.File, originExists func(context.Context, [32]byte, string, string) (bool, error), metadata, authorization string,
) {
	w.mu.Lock()
	_, exists := w.objects[key]
	w.mu.Unlock()

	if !exists && originExists != nil {
		var err error

		exists, err = originExists(r.Context(), key, metadata, authorization)
		if err != nil {
			// Racer answers a failed create-once probe as a busy coordinator.
			writeFailure(rw, WriteReply{Status: 503, Code: "coordinator", RetryAfter: 1})
			return
		}
	}

	if exists {
		writeFailure(rw, WriteReply{Status: 412})
		return
	}

	var (
		data []byte
		err  error
	)

	if kind == wire.WritePut {
		data = make([]byte, r.ContentLength)
		_, err = io.ReadFull(r.Body, data)
	} else {
		data, err = readSource(r.Header, files[0])
	}

	if err != nil {
		writeFailure(rw, WriteReply{Status: 400})
		return
	}

	var nonce [16]byte

	_, _ = rand.Read(nonce[:]) //nolint:errcheck // crypto/rand.Read never fails.

	spec.Data = data
	spec.ETag = `"racer-` + hex.EncodeToString(nonce[:]) + `"`
	spec.Metadata = metadata
	spec.Created = time.Now()

	switch {
	case spec.OriginDurable:
		spec.State = wire.StateCommitted
	case spec.Upload == wire.UploadAuto:
		spec.State = wire.StatePending
	case spec.Upload == wire.UploadManual:
		spec.State = wire.StateManual
	default:
		spec.State = wire.StateNever
	}

	w.mu.Lock()
	if _, raced := w.objects[key]; raced {
		w.mu.Unlock()
		writeFailure(rw, WriteReply{Status: 412})

		return
	}

	w.objects[key] = &spec
	w.mu.Unlock()

	hdr := rw.Header()
	hdr.Set("Etag", spec.ETag)
	hdr.Set("Racer-Upload-State", spec.State)
	hdr.Set("Content-Length", "0")
	rw.WriteHeader(201)
}

func readSource(h http.Header, file *os.File) ([]byte, error) {
	offset, err := wire.Decimal(h.Get("Racer-Source-Offset"))
	if err != nil {
		return nil, err
	}

	length, err := wire.Decimal(h.Get("Racer-Source-Length"))
	if err != nil || length == 0 || length > wire.MaxObjectBytes {
		return nil, errors.New("invalid source length")
	}

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || uint64(info.Size()) < offset+length {
		return nil, errors.New("invalid source")
	}

	data := make([]byte, length)
	if _, err := file.ReadAt(data, int64(offset)); err != nil {
		return nil, err
	}

	return data, nil
}

func (w *Writes) persist(rw http.ResponseWriter, key [32]byte) {
	w.mu.Lock()
	o, ok := w.objects[key]

	reply := WriteReply{Status: 404}
	state := ""

	switch {
	case !ok:
	case o.State == wire.StateManual:
		o.State = wire.StatePending
		reply.Status, state = 204, o.State
	case o.State == wire.StateNever || o.State == wire.StateCommitted:
		reply = WriteReply{Status: 409, Code: "not-manual"}
	default:
		reply.Status, state = 204, o.State
	}
	w.mu.Unlock()

	if reply.Status != 204 {
		writeFailure(rw, reply)
		return
	}

	rw.Header().Set("Racer-Upload-State", state)
	rw.WriteHeader(204)
}

func (w *Writes) remove(rw http.ResponseWriter, key [32]byte) {
	w.mu.Lock()
	o, ok := w.objects[key]

	reply := WriteReply{Status: 204}

	switch {
	case !ok:
		reply.Status = 404
	case o.State == wire.StateCommitted:
		reply = WriteReply{Status: 409, Code: "uploaded"}
	default:
		delete(w.objects, key)
	}
	w.mu.Unlock()

	if reply.Status != 204 {
		writeFailure(rw, reply)
		return
	}

	rw.WriteHeader(204)
}

type filesKey struct{}

// FileConn is a Unix connection that collects descriptors received with
// SCM_RIGHTS, so the fake can serve host-read and host-write.
type FileConn struct {
	*net.UnixConn

	mu    sync.Mutex
	files []*os.File
}

// Read reads like UnixConn.Read and keeps any descriptors that arrive.
func (c *FileConn) Read(p []byte) (int, error) {
	oob := make([]byte, syscall.CmsgSpace(4*4))

	n, oobn, _, _, err := c.ReadMsgUnix(p, oob)
	n = max(n, 0)

	if oobn > 0 {
		msgs, perr := syscall.ParseSocketControlMessage(oob[:oobn])
		if perr == nil {
			for i := range msgs {
				fds, ferr := syscall.ParseUnixRights(&msgs[i])
				if ferr != nil {
					continue
				}

				c.mu.Lock()
				for _, fd := range fds {
					c.files = append(c.files, os.NewFile(uintptr(fd), "client"))
				}
				c.mu.Unlock()
			}
		}
	}

	return n, err
}

// TakeFiles returns and forgets the descriptors received so far.
func (c *FileConn) TakeFiles() []*os.File {
	c.mu.Lock()
	defer c.mu.Unlock()

	files := c.files
	c.files = nil

	return files
}

// Close closes the connection and any descriptors not taken.
func (c *FileConn) Close() error {
	for _, f := range c.TakeFiles() {
		closeBody(f)
	}

	return c.UnixConn.Close()
}

// FileListener wraps a Unix listener so each accepted connection is a
// [*FileConn].
type FileListener struct{ *net.UnixListener }

// Accept accepts a [*FileConn].
func (l FileListener) Accept() (net.Conn, error) {
	c, err := l.AcceptUnix()
	if err != nil {
		return nil, err
	}

	return &FileConn{UnixConn: c}, nil
}

// ConnContext stores a [*FileConn] in the request context; use it as
// http.Server.ConnContext.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	if fc, ok := c.(*FileConn); ok {
		return context.WithValue(ctx, filesKey{}, fc)
	}

	return ctx
}

func takeFiles(ctx context.Context) []*os.File {
	fc, ok := ctx.Value(filesKey{}).(*FileConn)
	if !ok {
		return nil
	}

	return fc.TakeFiles()
}
