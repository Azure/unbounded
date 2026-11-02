// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
)

func TestLSMTImageReadsHolesZerosAndTopLayerOverrides(t *testing.T) {
	bottomBytes := makeLSMTLayer(t, 4*sectorSize, []testMapping{
		{logical: 0, data: bytes.Repeat([]byte{'a'}, int(sectorSize))},
		{logical: 1, data: bytes.Repeat([]byte{'b'}, int(sectorSize))},
	})
	topBytes := makeLSMTLayer(t, 4*sectorSize, []testMapping{
		{logical: 1, data: bytes.Repeat([]byte{'x'}, int(sectorSize))},
		{logical: 2, zeroed: true},
	})

	bottom, err := OpenLSMTLayer(bytes.NewReader(bottomBytes), int64(len(bottomBytes)))
	if err != nil {
		t.Fatal(err)
	}

	top, err := OpenLSMTLayer(bytes.NewReader(topBytes), int64(len(topBytes)))
	if err != nil {
		t.Fatal(err)
	}

	image, err := NewLSMTImage(bottom, top)
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 4*sectorSize)
	if n, err := image.ReadAt(buffer, 0); err != nil || n != len(buffer) {
		t.Fatalf("ReadAt = %d, %v", n, err)
	}

	if !allByte(buffer[:sectorSize], 'a') || !allByte(buffer[sectorSize:2*sectorSize], 'x') || !allByte(buffer[2*sectorSize:], 0) {
		t.Fatal("merged sectors do not match lower, override, zero, hole order")
	}
}

func TestLSMTImageSupportsUnalignedReadsAndEOF(t *testing.T) {
	layerBytes := makeLSMTLayer(t, 2*sectorSize, []testMapping{{logical: 0, data: bytes.Repeat([]byte{'z'}, int(2*sectorSize))}})

	layer, err := OpenLSMTLayer(bytes.NewReader(layerBytes), int64(len(layerBytes)))
	if err != nil {
		t.Fatal(err)
	}

	image, err := NewLSMTImage(layer)
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 20)
	if n, err := image.ReadAt(buffer, sectorSize-10); err != nil || n != len(buffer) || !allByte(buffer, 'z') {
		t.Fatalf("unaligned ReadAt = %d, %q, %v", n, buffer, err)
	}

	buffer = make([]byte, 20)

	n, err := image.ReadAt(buffer, 2*sectorSize-10)
	if n != 10 || err != io.EOF || !allByte(buffer[:n], 'z') {
		t.Fatalf("EOF ReadAt = %d, %q, %v", n, buffer[:n], err)
	}
}

func TestLSMTImageCoalescesContiguousPhysicalReads(t *testing.T) {
	layerBytes := makeLSMTLayer(t, 4*sectorSize, []testMapping{{logical: 0, data: bytes.Repeat([]byte{'z'}, int(4*sectorSize))}})
	reader := &countingReaderAt{reader: bytes.NewReader(layerBytes)}

	layer, err := OpenLSMTLayer(reader, int64(len(layerBytes)))
	if err != nil {
		t.Fatal(err)
	}

	image, err := NewLSMTImage(layer)
	if err != nil {
		t.Fatal(err)
	}

	reader.reads = 0

	buffer := make([]byte, 3*sectorSize)
	if _, err := image.ReadAt(buffer, sectorSize/2); err != nil {
		t.Fatal(err)
	}

	if reader.reads != 1 {
		t.Fatalf("physical reads = %d, want 1", reader.reads)
	}
}

func TestLSMTImageUsesHighestNonzeroVirtualSize(t *testing.T) {
	bottomBytes := makeLSMTLayer(t, 0, []testMapping{{logical: 0, data: bytes.Repeat([]byte{'b'}, int(sectorSize))}})
	topBytes := makeLSMTLayer(t, 2*sectorSize, nil)

	bottom, err := OpenLSMTLayer(bytes.NewReader(bottomBytes), int64(len(bottomBytes)))
	if err != nil {
		t.Fatal(err)
	}

	top, err := OpenLSMTLayer(bytes.NewReader(topBytes), int64(len(topBytes)))
	if err != nil {
		t.Fatal(err)
	}

	image, err := NewLSMTImage(bottom, top)
	if err != nil {
		t.Fatal(err)
	}

	if image.Size() != 2*sectorSize {
		t.Fatalf("image size = %d", image.Size())
	}
}

func TestOpenLSMTLayerRejectsCorruptMetadata(t *testing.T) {
	valid := makeLSMTLayer(t, sectorSize, []testMapping{{logical: 0, data: bytes.Repeat([]byte{'a'}, int(sectorSize))}})

	for _, test := range []struct {
		name string
		edit func([]byte)
	}{
		{name: "magic", edit: func(data []byte) { data[0] = 0 }},
		{name: "unsealed trailer", edit: func(data []byte) { binary.LittleEndian.PutUint32(data[len(data)-int(headerSize)+28:], 2) }},
		{name: "index outside file", edit: func(data []byte) {
			binary.LittleEndian.PutUint64(data[len(data)-int(headerSize)+32:], uint64(len(data)))
		}},
		{name: "unsupported version", edit: func(data []byte) { data[132] = 2 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := append([]byte(nil), valid...)
			test.edit(data)

			if _, err := OpenLSMTLayer(bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("corrupt layer accepted")
			}
		})
	}
}

type testMapping struct {
	logical uint64
	data    []byte
	zeroed  bool
}

type countingReaderAt struct {
	reader io.ReaderAt
	reads  int
}

func (r *countingReaderAt) ReadAt(buffer []byte, offset int64) (int, error) {
	r.reads++
	return r.reader.ReadAt(buffer, offset)
}

func makeLSMTLayer(t *testing.T, virtualSize int64, mappings []testMapping) []byte {
	t.Helper()

	data := make([]byte, headerSize)
	entries := make([]Mapping, 0, len(mappings))
	physicalSector := uint64(headerSize / sectorSize)

	for _, mapping := range mappings {
		length := uint32(1)

		if len(mapping.data) != 0 {
			if len(mapping.data)%int(sectorSize) != 0 {
				t.Fatalf("test data length %d is not sector aligned", len(mapping.data))
			}

			length = uint32(len(mapping.data) / int(sectorSize))
		}

		entry := Mapping{Offset: mapping.logical, Length: length, MOffset: physicalSector, Zeroed: mapping.zeroed}

		entries = append(entries, entry)
		if !mapping.zeroed {
			data = append(data, mapping.data...)
			physicalSector += uint64(length)
		}
	}

	indexOffset := len(data)
	for _, mapping := range entries {
		data = append(data, encodeMapping(mapping)...)
	}

	trailerOffset := len(data)
	data = append(data, make([]byte, headerSize)...)

	writeHeaderTrailer(data[:headerSize], true, 0, 0, uint64(virtualSize))
	writeHeaderTrailer(data[trailerOffset:], false, uint64(indexOffset), uint64(len(entries)), uint64(virtualSize))

	return data
}

func writeHeaderTrailer(buffer []byte, header bool, indexOffset, indexCount, virtualSize uint64) {
	copy(buffer[:8], magic0[:])
	copy(buffer[8:24], magic1[:])
	binary.LittleEndian.PutUint32(buffer[24:28], 390)

	flags := uint32(2 | 4)
	if header {
		flags |= 1
	}

	binary.LittleEndian.PutUint32(buffer[28:32], flags)
	binary.LittleEndian.PutUint64(buffer[32:40], indexOffset)
	binary.LittleEndian.PutUint64(buffer[40:48], indexCount)
	binary.LittleEndian.PutUint64(buffer[48:56], virtualSize)
	buffer[132] = 1
	buffer[133] = 1
}

func encodeMapping(mapping Mapping) []byte {
	data := make([]byte, mappingSize)
	logical := mapping.Offset | uint64(mapping.Length)<<50

	physical := mapping.MOffset
	if mapping.Zeroed {
		physical |= 1 << 55
	}

	binary.LittleEndian.PutUint64(data[:8], logical)
	binary.LittleEndian.PutUint64(data[8:], physical)

	return data
}

func allByte(data []byte, value byte) bool {
	for _, current := range data {
		if current != value {
			return false
		}
	}

	return true
}
