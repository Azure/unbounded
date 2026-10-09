//go:build e2e

// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racer_test

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// The probes request at most one page. Completion retires its final lease;
// unlike a multipage client they do not need to return credits to make progress.
func singlePageSubscription(r *http.Response) ([]byte, error) {
	bad := fmt.Errorf("invalid single-page subscription")
	field := func(name string) (uint64, error) { return strconv.ParseUint(r.Header.Get(name), 10, 63) }

	size, err := field("Racer-Object-Length")
	if err != nil {
		return nil, bad
	}

	start, err := field("Racer-Range-Start")
	if err != nil {
		return nil, bad
	}

	end, err := field("Racer-Range-End")
	if err != nil || start > end || end > size || end-start > peerPageSize || (end > start && start/peerPageSize != (end-1)/peerPageSize) {
		return nil, bad
	}

	pages := uint64(0)
	if end > start {
		pages = 1
	}

	if r.StatusCode != http.StatusOK || !r.Close || r.ContentLength != int64(end-start+21*(pages+1)) || r.Header.Get("Content-Range") != "" || r.Header.Get("Content-Type") != "application/octet-stream" || r.Header.Get("ETag") == "" {
		return nil, bad
	}

	if _, err := field("Racer-Expires-At"); err != nil {
		return nil, bad
	}

	var frame [21]byte

	body := make([]byte, end-start)

	if pages != 0 {
		if _, err := io.ReadFull(r.Body, frame[:]); err != nil {
			return nil, err
		}

		if frame[0] != 1 || binary.BigEndian.Uint64(frame[1:9]) != start/peerPageSize || binary.BigEndian.Uint64(frame[9:17]) != start || uint64(binary.BigEndian.Uint32(frame[17:])) != end-start {
			return nil, bad
		}

		if _, err := io.ReadFull(r.Body, body); err != nil {
			return nil, err
		}
	}

	if _, err := io.ReadFull(r.Body, frame[:]); err != nil {
		return nil, err
	}

	if frame[0] != 2 || binary.BigEndian.Uint64(frame[1:9]) != pages || binary.BigEndian.Uint64(frame[9:17]) != end-start || binary.BigEndian.Uint32(frame[17:]) != 0 {
		return nil, bad
	}

	var extra [1]byte
	if n, err := r.Body.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, bad
	}

	return body, nil
}

func TestSinglePageSubscriptionFrames(t *testing.T) {
	frame := func(kind byte, number, offset uint64, length uint32) []byte {
		b := make([]byte, 21)
		b[0] = kind
		binary.BigEndian.PutUint64(b[1:9], number)
		binary.BigEndian.PutUint64(b[9:17], offset)
		binary.BigEndian.PutUint32(b[17:], length)

		return b
	}
	valid := append(append(frame(1, 1, peerPageSize+3, 3), []byte("abc")...), frame(2, 1, 3, 0)...)

	for _, name := range []string{"valid", "empty", "truncated", "missing-completion", "wrong-page", "wrong-completion", "trailing", "metadata"} {
		t.Run(name, func(t *testing.T) {
			body := bytes.Clone(valid)
			r := &http.Response{StatusCode: 200, Close: true, ContentLength: int64(len(valid)), Header: http.Header{
				"Content-Type": {"application/octet-stream"}, "Etag": {`"v"`}, "Racer-Expires-At": {"0"}, "Racer-Object-Length": {"33554432"}, "Racer-Range-Start": {"16777219"}, "Racer-Range-End": {"16777222"},
			}}

			switch name {
			case "empty":
				body = frame(2, 0, 0, 0)
				r.ContentLength = 21
				r.Header.Set("Racer-Object-Length", "0")
				r.Header.Set("Racer-Range-Start", "0")
				r.Header.Set("Racer-Range-End", "0")
			case "truncated":
				body = body[:22]
			case "missing-completion":
				body = body[:24]
			case "wrong-page":
				body[8] = 2
			case "wrong-completion":
				body[32] = 2
			case "trailing":
				body = append(body, 0)
			case "metadata":
				r.Header.Set("Racer-Range-End", "16777223")
			}

			r.Body = io.NopCloser(bytes.NewReader(body))

			got, err := singlePageSubscription(r)

			switch name {
			case "valid":
				require.NoError(t, err)
				require.Equal(t, []byte("abc"), got)
			case "empty":
				require.NoError(t, err)
				require.Empty(t, got)
			default:
				require.Error(t, err)
			}
		})
	}
}
