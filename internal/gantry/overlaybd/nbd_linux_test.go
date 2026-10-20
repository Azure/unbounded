// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
)

func TestServeNBDReadAndRejectWrite(t *testing.T) {
	server, client := net.Pipe()
	disk := &memoryDisk{data: []byte("0123456789")}

	done := make(chan error, 1)
	go func() { done <- serveNBD(server, disk) }()

	handle := [8]byte{1, 2, 3, 4, 5, 6, 7, 8}
	writeNBDRequest(t, client, nbdCommandRead, handle, 2, 4, nil)

	reply := readNBDReply(t, client, 4)
	if reply.errno != 0 || reply.handle != handle || string(reply.data) != "2345" {
		t.Fatalf("read reply = %#v", reply)
	}

	writeNBDRequest(t, client, nbdCommandWrite, handle, 0, 3, []byte("new"))

	reply = readNBDReply(t, client, 0)
	if reply.errno == 0 || string(disk.data) != "0123456789" {
		t.Fatalf("write reply = %#v, disk = %q", reply, disk.data)
	}

	writeNBDRequest(t, client, nbdCommandDisconnect, handle, 0, 0, nil)

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestServeNBDRejectsOutOfBoundsRead(t *testing.T) {
	server, client := net.Pipe()

	done := make(chan error, 1)
	go func() { done <- serveNBD(server, &memoryDisk{data: []byte("small")}) }()

	handle := [8]byte{9}
	writeNBDRequest(t, client, nbdCommandRead, handle, 4, 2, nil)

	reply := readNBDReply(t, client, 0)
	if reply.errno == 0 {
		t.Fatal("out-of-bounds read succeeded")
	}

	writeNBDRequest(t, client, nbdCommandDisconnect, handle, 0, 0, nil)

	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type memoryDisk struct{ data []byte }

func (d *memoryDisk) Size() int64 { return int64(len(d.data)) }

func (d *memoryDisk) ReadAt(buffer []byte, offset int64) (int, error) {
	if offset >= int64(len(d.data)) {
		return 0, io.EOF
	}

	n := copy(buffer, d.data[offset:])
	if n != len(buffer) {
		return n, io.EOF
	}

	return n, nil
}

type nbdTestReply struct {
	errno  uint32
	handle [8]byte
	data   []byte
}

func writeNBDRequest(t *testing.T, writer io.Writer, command uint32, handle [8]byte, offset uint64, length uint32, data []byte) {
	t.Helper()

	request := make([]byte, nbdRequestSize)
	binary.BigEndian.PutUint32(request[:4], nbdRequestMagic)
	binary.BigEndian.PutUint32(request[4:8], command)
	copy(request[8:16], handle[:])
	binary.BigEndian.PutUint64(request[16:24], offset)
	binary.BigEndian.PutUint32(request[24:28], length)

	request = append(request, data...)
	if _, err := writer.Write(request); err != nil {
		t.Fatal(err)
	}
}

func readNBDReply(t *testing.T, reader io.Reader, length int) nbdTestReply {
	t.Helper()

	header := make([]byte, nbdReplySize)
	if _, err := io.ReadFull(reader, header); err != nil {
		t.Fatal(err)
	}

	if magic := binary.BigEndian.Uint32(header[:4]); magic != nbdReplyMagic {
		t.Fatalf("reply magic = %#x", magic)
	}

	reply := nbdTestReply{errno: binary.BigEndian.Uint32(header[4:8]), data: make([]byte, length)}
	copy(reply.handle[:], header[8:16])

	if _, err := io.ReadFull(reader, reply.data); err != nil {
		t.Fatal(err)
	}

	return reply
}

func TestWriteNBDReply(t *testing.T) {
	var output bytes.Buffer
	if err := writeNBDReply(&output, []byte("12345678"), 0, []byte("data")); err != nil {
		t.Fatal(err)
	}

	if output.Len() != nbdReplySize+4 {
		t.Fatalf("reply length = %d", output.Len())
	}
}
