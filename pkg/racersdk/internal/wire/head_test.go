// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestRawHeaders(t *testing.T) {
	for _, field := range []string{"Host", "Content-Length", "Content-Type", "Content-Range", "ETag", "If-Match", "Range", "Racer-Expires-At", "Racer-Metadata", "Authorization"} {
		for _, second := range []string{"same", "different"} {
			head := []byte("HEAD / HTTP/1.1\r\n" + field + ": same\r\n" + strings.ToLower(field) + ": " + second + "\r\n\r\n")
			assertKind(t, ValidateRawHead(head, false), ErrorInvalidArgument)
			assertKind(t, ValidateRawHead(head, true), ErrorProtocol)
		}
	}

	for _, value := range []string{"", "x", "  x", " x ", "\tx", " x\t", " a\r\nb", " a\x00b", " a\x7fb"} {
		for _, field := range []string{"Authorization", "Racer-Metadata"} {
			err := ValidateRawHead(rawRequest("HEAD", field+":"+value+"\r\n"), false)
			if err == nil {
				t.Fatalf("accepted malformed %s", field)
			}
		}
	}

	for _, line := range []string{" Folded: x", "X : x", "X\t: x", "X: a\nb", "X: a\rb", ": x"} {
		if err := ValidateRawHead(rawRequest("HEAD", line+"\r\n"), false); err == nil {
			t.Fatal("accepted malformed field")
		}
	}

	for _, count := range []int{8192, 8193} {
		err := ValidateRawHead(rawRequest("HEAD", "Authorization: "+strings.Repeat("a", count)+"\r\n"), false)
		if count == 8192 {
			if err != nil {
				t.Fatal(err)
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	base := rawRequest("HEAD", "X: \r\n")
	for _, size := range []int{MaxHeadBytes - 1, MaxHeadBytes, MaxHeadBytes + 1} {
		head := rawRequest("HEAD", "X: "+strings.Repeat("x", size-len(base))+"\r\n")

		got, err := ReadRawHead(bufio.NewReader(bytes.NewReader(head)), false)
		if size <= MaxHeadBytes {
			if err != nil || !bytes.Equal(got, head) {
				t.Fatal("head boundary")
			}
		} else {
			assertKind(t, err, ErrorHeaderLimit)
		}
	}

	_, err := ReadRawHead(bufio.NewReader(strings.NewReader("HTTP/1.1")), true)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncated head")
	}

	_, err = ReadRawHead(bufio.NewReader(strings.NewReader("")), false)
	if err != io.EOF {
		t.Fatal("idle EOF")
	}
}

func TestStdlibNormalizationRequiresRawValidation(t *testing.T) {
	head := rawRequest("HEAD", "Authorization:  secret \r\nContent-Length: 0\r\nContent-Length: 0\r\n")

	req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(head)))
	if err != nil {
		t.Fatal(err)
	}

	if req.Header.Get("Authorization") != "secret" || len(req.Header.Values("Content-Length")) != 1 {
		t.Fatal("stdlib normalization changed; revisit guard assumptions")
	}

	if err := ValidateRawHead(head, false); err == nil {
		t.Fatal("guard trusted normalized headers")
	}
}

type finalErrorReader struct{ err error }

func (r finalErrorReader) Read(p []byte) (int, error) { return copy(p, "abc"), r.err }

func TestFrameReaderBoundaries(t *testing.T) {
	body := "body\r\n\r\nnot a head"
	firstHead := rawResponse(200, "Content-Length: "+strconv.Itoa(len(body))+"\r\n")
	nextHead := rawResponse(503, "Content-Length: 0\r\n")

	source := bufio.NewReader(strings.NewReader(string(firstHead) + body + string(nextHead)))
	if _, err := ReadRawHead(source, true); err != nil {
		t.Fatal(err)
	}

	frame := &FrameReader{Source: source, Remaining: int64(len(body))}

	got, err := io.ReadAll(frame)
	if err != nil || string(got) != body {
		t.Fatal("body corrupted")
	}

	head, err := ReadRawHead(source, true)
	if err != nil || !bytes.Equal(head, nextHead) {
		t.Fatal("read-ahead lost or body scanned")
	}

	frame = &FrameReader{Source: strings.NewReader("abc"), Remaining: 4}

	got, err = io.ReadAll(frame)
	if string(got) != "abc" || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("truncation lost")
	}

	cause := errors.New("late private failure")
	frame = &FrameReader{Source: finalErrorReader{err: cause}, Remaining: 3}

	n, err := frame.Read(make([]byte, 8))
	if n != 3 || !errors.Is(err, cause) {
		t.Fatal("final error lost")
	}

	frame = &FrameReader{Source: finalErrorReader{err: io.EOF}, Remaining: 3}

	got, err = io.ReadAll(frame)
	if err != nil || string(got) != "abc" {
		t.Fatal("exact EOF")
	}

	frame = &FrameReader{Remaining: 0}
	if n, err := frame.Read(nil); n != 0 || err != nil {
		t.Fatal("empty read")
	}

	if n, err := frame.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatal("zero frame")
	}
}
