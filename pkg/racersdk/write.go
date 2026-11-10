// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"context"
	"errors"
	"io"
	"math"
	"os"
	"strconv"
	"time"

	"github.com/Azure/unbounded/pkg/racersdk/internal/wire"
)

// UploadMode selects whether and when Racer uploads a written object to the
// cache's origin. The zero value selects the cache default.
type UploadMode string

// Upload modes.
const (
	// UploadAuto uploads the object after it is durable in the cache.
	UploadAuto UploadMode = wire.UploadAuto
	// UploadManual keeps the object in the cache until [Client.Persist].
	UploadManual UploadMode = wire.UploadManual
	// UploadNever keeps the object only in the cache until its hold ends.
	UploadNever UploadMode = wire.UploadNever
)

// UploadState is the upload progress of a written object, as reported by
// Racer-Upload-State.
type UploadState string

// Upload states.
const (
	// UploadStatePending waits to upload.
	UploadStatePending UploadState = wire.StatePending
	// UploadStateUploading is uploading.
	UploadStateUploading UploadState = wire.StateUploading
	// UploadStateCommitted is uploaded to the origin.
	UploadStateCommitted UploadState = wire.StateCommitted
	// UploadStateFailed failed to upload; the object stays pinned until its hold ends.
	UploadStateFailed UploadState = wire.StateFailed
	// UploadStateNever is cache-only.
	UploadStateNever UploadState = wire.StateNever
	// UploadStateManual waits for [Client.Persist].
	UploadStateManual UploadState = wire.StateManual
)

// MaxWriteSize is the largest object [Client.Put] and [Client.HostWrite]
// accept: 50,000 pages, the Azure Blob block limit.
const MaxWriteSize = wire.MaxObjectBytes

// MaxHold is the longest hold a write may request (30 days).
const MaxHold = wire.MaxHoldSeconds * time.Second

// WriteOptions control one write. The zero value uses the cache defaults.
type WriteOptions struct {
	// Replicas is how many durable cache copies Racer makes before it
	// acknowledges the write, 1 to 3. Zero means the cache default.
	Replicas int
	// OriginDurable acknowledges only after the origin upload commits. It
	// excludes Replicas and requires an automatic upload.
	OriginDurable bool
	// Upload selects the upload mode. Empty means the cache default.
	Upload UploadMode
	// Hold is how long Racer pins the object, rounded up to whole seconds,
	// at most [MaxHold]. Zero means the cache default.
	Hold time.Duration
	// ContentType is stored with the object and returned by [Client.Stat].
	ContentType string
	// RetryBusy, when positive, retries a write that Racer rejected with
	// [ErrBusy] for up to this long, waiting the Retry-After interval
	// between attempts. Put retries only when the body is an
	// [io.ReadSeeker], so it can rewind it. Zero never retries.
	RetryBusy time.Duration
}

// WriteResult reports a completed write.
type WriteResult struct {
	// ETag is the Racer version minted for the object.
	ETag string
	// UploadState is the upload state when the write was acknowledged.
	UploadState UploadState
}

// Write errors. A [*WriteError] matches the sentinels for its status; test
// with [errors.Is] and inspect details with [errors.As].
var (
	// ErrExists reports that the key already exists in the cache or at the
	// origin. Writes are create-once.
	ErrExists = errors.New("racersdk: object exists")
	// ErrWritesDisabled reports that the cache does not accept writes, or
	// that Racer predates writes.
	ErrWritesDisabled = errors.New("racersdk: writes disabled")
	// ErrBusy reports that Racer cannot accept the write now, for example
	// because the dirty cap is reached. It also matches [ErrUnavailable].
	ErrBusy = errors.New("racersdk: busy")
	// ErrUploadFailed reports that the origin upload failed under
	// [WriteOptions.OriginDurable].
	ErrUploadFailed = errors.New("racersdk: upload failed")
	// ErrConflict reports that the object's state does not allow the
	// operation, such as persisting an object that is not manual or
	// deleting one that is already uploaded.
	ErrConflict = errors.New("racersdk: conflict")
)

// WriteError is a write rejected by Racer.
type WriteError struct {
	// Op is the operation: put, host-write, persist, or delete.
	Op string
	// Status is the HTTP status.
	Status int
	// Code is the Racer-Write-Error diagnostic token, if any.
	Code string
	// RetryAfter is the server's Retry-After hint, or zero.
	RetryAfter time.Duration
}

func (e *WriteError) Error() string {
	msg := "racersdk: " + e.Op + ": HTTP " + strconv.Itoa(e.Status)
	if e.Code != "" {
		msg += " (" + e.Code + ")"
	}

	return msg
}

// Is matches the sentinel errors for e's status.
func (e *WriteError) Is(target error) bool {
	switch e.Status {
	case 400, 411, 413, 428, 431:
		return target == ErrInvalidRequest
	case 401:
		return target == ErrUnauthorized
	case 403:
		return target == ErrForbidden
	case 404:
		return target == ErrNotFound
	case 405:
		return target == ErrWritesDisabled
	case 409:
		return target == ErrConflict
	case 412:
		return target == ErrExists
	case 502:
		return target == ErrUploadFailed
	case 503:
		return target == ErrBusy || target == ErrUnavailable
	case 504:
		return target == ErrUnavailable
	default:
		return false
	}
}

func (o WriteOptions) wire(length, offset int64) (wire.WriteOptions, error) {
	if o.Hold < 0 || o.Hold > MaxHold || o.RetryBusy < 0 {
		return wire.WriteOptions{}, errors.New("invalid hold or retry")
	}

	hold := uint64((o.Hold + time.Second - 1) / time.Second)

	return wire.WriteOptions{
		Replicas:      o.Replicas,
		OriginDurable: o.OriginDurable,
		Upload:        string(o.Upload),
		HoldSeconds:   hold,
		ContentType:   o.ContentType,
		Length:        uint64(length),
		SourceOffset:  uint64(offset),
	}, nil
}

// Put creates an object from length bytes of body. Writes are create-once:
// if the key exists Put fails with [ErrExists]. Put returns after Racer
// reports the object durable at the requested level and visible to readers.
// Request.Metadata must not name a version.
func (c *Client) Put(ctx context.Context, request Request, body io.Reader, length int64, options WriteOptions) (WriteResult, error) {
	const op = "put"

	if body == nil {
		return WriteResult{}, invalid(op, errors.New("nil body"))
	}

	if length <= 0 || length > MaxWriteSize {
		return WriteResult{}, invalid(op, errors.New("length must be in [1, MaxWriteSize]"))
	}

	seeker, canSeek := body.(io.ReadSeeker)
	if !canSeek {
		seeker = nil
	}

	start := int64(0)

	if seeker != nil && options.RetryBusy > 0 {
		pos, err := seeker.Seek(0, io.SeekCurrent)
		if err != nil {
			seeker = nil
		}

		start = pos
	}

	rewind := func() bool {
		if seeker == nil {
			return false
		}

		_, err := seeker.Seek(start, io.SeekStart)

		return err == nil
	}

	return c.write(ctx, op, wire.WritePut, request, options, length, 0, nil, body, rewind)
}

// HostWrite creates an object from length bytes of file starting at offset.
// The descriptor is passed to Racer with SCM_RIGHTS, so the caller and Racer
// must be on the same host. file must be a regular file or memfd readable by
// Racer; it must not change until HostWrite returns. Errors are as for
// [Client.Put].
func (c *Client) HostWrite(ctx context.Context, request Request, file *os.File, offset, length int64, options WriteOptions) (WriteResult, error) {
	const op = "host-write"

	if file == nil {
		return WriteResult{}, invalid(op, errors.New("nil file"))
	}

	if offset < 0 || length <= 0 || length > MaxWriteSize || offset > math.MaxInt64-length {
		return WriteResult{}, invalid(op, errors.New("invalid source window"))
	}

	return c.write(ctx, op, wire.WriteHostWrite, request, options, length, offset, file, nil, func() bool { return true })
}

// Persist starts the upload of a written object with upload mode
// [UploadManual] and returns its upload state.
func (c *Client) Persist(ctx context.Context, request Request) (UploadState, error) {
	result, err := c.write(ctx, "persist", wire.WritePersist, request, WriteOptions{}, 0, 0, nil, nil, nil)
	return result.UploadState, err
}

// Delete removes a written object from the cache. It never deletes origin
// objects; deleting an object whose upload committed fails with
// [ErrConflict].
func (c *Client) Delete(ctx context.Context, request Request) error {
	_, err := c.write(ctx, "delete", wire.WriteDelete, request, WriteOptions{}, 0, 0, nil, nil, nil)
	return err
}

func (c *Client) write(ctx context.Context, op, kind string, request Request, options WriteOptions,
	length, offset int64, file *os.File, body io.Reader, rewind func() bool,
) (WriteResult, error) {
	wo, err := options.wire(length, offset)
	if err != nil {
		return WriteResult{}, invalid(op, err)
	}

	r, err := request.wire(wire.OperationHead)
	if err != nil {
		return WriteResult{}, err
	}

	head, err := wire.WriteHead(kind, r, wo)
	if err != nil {
		return WriteResult{}, invalid(op, err)
	}

	l := &c.stat
	if kind == wire.WritePut || kind == wire.WriteHostWrite {
		l = &c.bulk
	}

	release, err := c.admit(ctx, l, op)
	if err != nil {
		return WriteResult{}, err
	}
	defer release()

	ctx, done := c.bind(ctx)
	defer done()

	deadline := time.Now().Add(options.RetryBusy)

	for {
		result, err := c.writeOnce(ctx, op, kind, head, file, body, length, options.OriginDurable)

		var we *WriteError
		if !errors.As(err, &we) || we.Status != 503 || options.RetryBusy <= 0 || rewind == nil {
			return result, err
		}

		wait := max(we.RetryAfter, time.Second)
		if time.Now().Add(wait).After(deadline) || !rewind() {
			return result, err
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return WriteResult{}, contextError(op, ctx)
		case <-timer.C:
		}
	}
}

func (c *Client) writeOnce(ctx context.Context, op, kind string, head []byte, file *os.File, body io.Reader, length int64, originDurable bool) (WriteResult, error) {
	conn, _, err := c.conn(ctx, op, true)
	if err != nil {
		return WriteResult{}, err
	}
	defer closeQuietly(conn)

	stop := context.AfterFunc(ctx, func() { closeQuietly(conn) })
	defer stop()

	fail := func(err error) (WriteResult, error) {
		if ctx.Err() != nil {
			return WriteResult{}, contextError(op, ctx)
		}

		return WriteResult{}, ioFailure(op, err)
	}

	if err := conn.SetDeadline(time.Now().Add(c.limits.headerTimeout)); err != nil {
		return fail(err)
	}

	if file != nil {
		err = sendWithFile(conn.Conn, head, file)
	} else {
		_, err = conn.Write(head)
	}

	// A send error may follow an early rejection; prefer the response.
	sendErr := err
	if sendErr == nil && body != nil {
		sendErr, err = c.sendBody(conn, body, length)
		if err != nil {
			return WriteResult{}, err
		}
	}

	timeout := c.limits.headerTimeout
	if originDurable {
		// Origin durability waits for the upload; only ctx bounds it.
		timeout = 0
	}

	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}

	if err := conn.SetDeadline(deadline); err != nil {
		return fail(err)
	}

	raw, err := wire.ReadRawHead(conn.r, true)
	if err != nil {
		if sendErr != nil {
			return fail(sendErr)
		}

		return fail(truncation(err))
	}

	response, err := wire.ParseWriteResponse(raw, kind)
	if err != nil {
		return fail(err)
	}

	switch response.Status {
	case 201, 204:
		if sendErr != nil {
			// Racer cannot have accepted a body it did not receive.
			return fail(sendErr)
		}

		return WriteResult{ETag: response.ETag, UploadState: UploadState(response.UploadState)}, nil
	default:
		we := &WriteError{Op: op, Status: response.Status, Code: response.Error}
		if response.RetryAfter >= 0 {
			we.RetryAfter = time.Duration(min(response.RetryAfter, 3600)) * time.Second
		}

		return WriteResult{}, we
	}
}

// sendBody copies length bytes of body to conn. It returns the connection
// error separately from a body failure: the first may hide an early response,
// the second is the caller's fault and ends the write.
func (c *Client) sendBody(conn *clientConn, body io.Reader, length int64) (connErr, bodyErr error) {
	const op = "put"

	buf := make([]byte, min(length, 1<<20))

	for sent := int64(0); sent < length; {
		n, err := body.Read(buf[:min(int64(len(buf)), length-sent)])
		if n > 0 {
			if err := conn.SetDeadline(time.Now().Add(c.limits.bodyTimeout)); err != nil {
				return err, nil
			}

			if _, err := conn.Write(buf[:n]); err != nil {
				return err, nil
			}

			sent += int64(n)
		}

		if sent == length {
			break
		}

		if err == io.EOF {
			return nil, invalid(op, errors.New("body is shorter than length"))
		}

		if err != nil {
			return nil, invalid(op, err)
		}
	}

	return nil, nil
}
