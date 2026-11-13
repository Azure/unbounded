// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func TestOpenLSMTLayerPayloadFormats(t *testing.T) {
	payload := makeLSMTLayer(t, 2*sectorSize, []testMapping{
		{logical: 0, data: bytes.Repeat([]byte{'a'}, int(2*sectorSize))},
	})

	for _, algorithm := range []byte{1, 2} {
		for _, envelope := range []string{"none", "ustar", "pax"} {
			t.Run(string(rune('0'+algorithm))+"/"+envelope, func(t *testing.T) {
				data := makeZFile(t, payload, algorithm, true)
				if envelope != "none" {
					data = wrapLayerTar(t, data, envelope == "pax")
				}

				layer, err := OpenLSMTLayer(bytes.NewReader(data), int64(len(data)))
				if err != nil {
					t.Fatal(err)
				}

				image, err := NewLSMTImage(layer)
				if err != nil {
					t.Fatal(err)
				}

				buffer := make([]byte, 2*sectorSize)
				if n, err := image.ReadAt(buffer, 0); err != nil || n != len(buffer) || !allByte(buffer, 'a') {
					t.Fatalf("ReadAt = %d, %v", n, err)
				}
			})
		}
	}

	data := wrapLayerTar(t, payload, true)
	if _, err := OpenLSMTLayer(bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatalf("uncompressed tar payload: %v", err)
	}
}

func TestZFileRandomAccess(t *testing.T) {
	payload := bytes.Repeat([]byte("random-access-test-"), 6000)

	for _, algorithm := range []byte{1, 2} {
		for _, checksum := range []bool{false, true} {
			data := makeZFile(t, payload, algorithm, checksum)

			reader, err := openZFile(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}

			for _, offset := range []int64{0, 1, 4095, 4096, 8191, int64(len(payload) - 10), int64(len(payload))} {
				buffer := make([]byte, 17000)
				n, err := reader.ReadAt(buffer, offset)

				want := min(len(buffer), len(payload)-int(offset))
				if n != want || !bytes.Equal(buffer[:n], payload[offset:offset+int64(want)]) {
					t.Fatalf("algorithm %d, checksum %t, offset %d: n=%d, want %d", algorithm, checksum, offset, n, want)
				}

				if want < len(buffer) && !errors.Is(err, io.EOF) || want == len(buffer) && err != nil {
					t.Fatalf("offset %d: error=%v", offset, err)
				}
			}

			if _, err := reader.ReadAt(make([]byte, 1), -1); err == nil {
				t.Fatal("negative read offset accepted")
			}

			if n, err := reader.ReadAt(nil, int64(len(payload))); n != 0 || err != nil {
				t.Fatalf("empty read = %d, %v", n, err)
			}
		}
	}
}

func TestZFileCoalescesReadsAndSupportsConcurrency(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 100*4096)
	data := makeZFile(t, payload, 1, true)
	source := &countingReaderAt{reader: bytes.NewReader(data)}

	reader, err := openZFile(source, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}

	source.reads = 0

	if _, err := reader.ReadAt(make([]byte, 3*4096), 7); err != nil {
		t.Fatal(err)
	}

	if source.reads != 1 {
		t.Fatalf("compressed reads = %d, want 1", source.reads)
	}

	reader.reader = bytes.NewReader(data)

	var workers sync.WaitGroup
	for i := range 8 {
		workers.Go(func() {
			offset := int64(i * 4095)
			buffer := make([]byte, 70*4096)

			n, err := reader.ReadAt(buffer, offset)
			if err != nil || n != len(buffer) || !bytes.Equal(buffer, payload[offset:offset+int64(n)]) {
				t.Errorf("concurrent ReadAt = %d, %v", n, err)
			}
		})
	}

	workers.Wait()
}

func TestZFileRejectsCorruption(t *testing.T) {
	payload := bytes.Repeat([]byte{'x'}, 9000)

	valid := makeZFile(t, payload, 1, true)
	for _, test := range []struct {
		name string
		edit func([]byte)
	}{
		{name: "magic", edit: func(data []byte) { data[0] = 0 }},
		{name: "header checksum", edit: func(data []byte) { data[28] ^= 1 }},
		{name: "trailer checksum", edit: func(data []byte) { data[len(data)-512+28] ^= 1 }},
		{name: "index checksum", edit: func(data []byte) {
			offset := binary.LittleEndian.Uint64(data[len(data)-512+40:])
			data[offset] ^= 1
		}},
		{name: "index range", edit: func(data []byte) {
			binary.LittleEndian.PutUint64(data[len(data)-512+40:], uint64(len(data)))
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "index allocation", edit: func(data []byte) {
			binary.LittleEndian.PutUint64(data[len(data)-512+48:], maxZFileBlocks+1)
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "unsealed trailer", edit: func(data []byte) {
			data[len(data)-512+32] &^= 4
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "compressed index", edit: func(data []byte) {
			data[len(data)-512+32] |= 32
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "invalid block size", edit: func(data []byte) {
			binary.LittleEndian.PutUint32(data[72:], 4095)
			binary.LittleEndian.PutUint32(data[len(data)-512+72:], 4095)
			sealZFileHeader(data[:512])
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "invalid original size", edit: func(data []byte) {
			binary.LittleEndian.PutUint64(data[len(data)-512+56:], 0)
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "changed options", edit: func(data []byte) {
			data[len(data)-512+76] = 2
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "checksum-only block", edit: func(data []byte) {
			trailer := data[len(data)-512:]
			offset := binary.LittleEndian.Uint64(trailer[40:])
			index := data[offset : offset+12]
			binary.LittleEndian.PutUint32(index, 4)
			binary.LittleEndian.PutUint32(trailer[64:], zfileChecksum(index, 0))
			sealZFileHeader(trailer)
		}},
		{name: "unsupported algorithm", edit: func(data []byte) {
			data[76], data[len(data)-512+76] = 0, 0
			sealZFileHeader(data[:512])
			sealZFileHeader(data[len(data)-512:])
		}},
		{name: "dictionary", edit: func(data []byte) {
			data[78], data[len(data)-512+78] = 1, 1
			sealZFileHeader(data[:512])
			sealZFileHeader(data[len(data)-512:])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := bytes.Clone(valid)
			test.edit(data)

			if _, err := openZFile(bytes.NewReader(data), int64(len(data))); err == nil {
				t.Fatal("corrupt ZFile accepted")
			}
		})
	}

	valid[512] ^= 1

	reader, err := openZFile(bytes.NewReader(valid), int64(len(valid)))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reader.ReadAt(make([]byte, 1), 0); err == nil {
		t.Fatal("corrupt block accepted")
	}
}

func TestZFileRejectsInvalidCompressionAndTruncation(t *testing.T) {
	for _, algorithm := range []byte{1, 2} {
		for _, malformed := range []bool{false, true} {
			data := makeZFile(t, bytes.Repeat([]byte{'x'}, 4096), algorithm, false)

			trailer := data[len(data)-512:]
			if malformed {
				indexOffset := binary.LittleEndian.Uint64(trailer[40:])
				clear(data[512:indexOffset])
			} else {
				binary.LittleEndian.PutUint64(trailer[56:], 4095)
				sealZFileHeader(trailer)
			}

			reader, err := openZFile(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				t.Fatal(err)
			}

			if _, err := reader.ReadAt(make([]byte, 1), 0); err == nil {
				t.Fatalf("algorithm %d accepted malformed=%t block", algorithm, malformed)
			}
		}
	}

	data := makeZFile(t, bytes.Repeat([]byte{'x'}, 9000), 1, true)
	for _, size := range []int{0, 511, 1023, len(data) - 1} {
		if _, err := openZFile(bytes.NewReader(data[:size]), int64(size)); err == nil {
			t.Fatalf("truncated size %d accepted", size)
		}
	}
}

func TestLayerTarRejectsUnexpectedEntries(t *testing.T) {
	var archive bytes.Buffer

	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{Name: "../other", Typeflag: tar.TypeReg, Size: 4096}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := openLayerPayload(bytes.NewReader(archive.Bytes()), int64(archive.Len())); err == nil {
		t.Fatal("unexpected tar entry accepted")
	}
}

func TestACRLayerFixture(t *testing.T) {
	path := os.Getenv("GANTRY_ACR_LAYER_FIXTURE")
	if path == "" {
		t.Skip("set GANTRY_ACR_LAYER_FIXTURE to validate a captured ACR blob")
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}

	layer, err := OpenLSMTLayer(file, info.Size())
	if err != nil {
		t.Fatal(err)
	}

	image, err := NewLSMTImage(layer)
	if err != nil {
		t.Fatal(err)
	}

	buffer := make([]byte, 4096)
	if _, err := image.ReadAt(buffer, 0); err != nil {
		t.Fatal(err)
	}

	if binary.LittleEndian.Uint16(buffer[1024+56:]) != 0xef53 {
		t.Fatal("real ACR base layer does not expose an ext4 filesystem")
	}
}

func wrapLayerTar(t *testing.T, payload []byte, pax bool) []byte {
	t.Helper()

	var buffer bytes.Buffer

	writer := tar.NewWriter(&buffer)

	header := &tar.Header{Name: "overlaybd.commit", Typeflag: tar.TypeReg, Size: int64(len(payload)), Mode: 0o644, Format: tar.FormatUSTAR}
	if pax {
		header.Format = tar.FormatPAX
		header.PAXRecords = map[string]string{"comment": "ACR layer"}
	}

	if err := writer.WriteHeader(header); err != nil {
		t.Fatal(err)
	}

	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}

	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	return buffer.Bytes()
}

func makeZFile(t *testing.T, payload []byte, algorithm byte, checksum bool) []byte {
	t.Helper()

	data := make([]byte, 512)

	var lengths []uint32

	for offset := 0; offset < len(payload); offset += 4096 {
		raw := payload[offset:min(offset+4096, len(payload))]

		var compressed []byte
		if algorithm == 1 {
			compressed = make([]byte, lz4.CompressBlockBound(len(raw)))

			n, err := lz4.CompressBlock(raw, compressed, nil)
			if err != nil || n == 0 {
				t.Fatalf("compress LZ4 fixture: %d, %v", n, err)
			}

			compressed = compressed[:n]
		} else {
			encoder, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
			if err != nil {
				t.Fatal(err)
			}

			compressed = encoder.EncodeAll(raw, nil)
			encoder.Close()
		}

		if checksum {
			compressed = binary.LittleEndian.AppendUint32(compressed, zfileChecksum(compressed, 100007))
		}

		lengths = append(lengths, uint32(len(compressed)))
		data = append(data, compressed...)
	}

	indexOffset := len(data)
	for _, length := range lengths {
		data = binary.LittleEndian.AppendUint32(data, length)
	}

	indexCRC := zfileChecksum(data[indexOffset:], 0)

	data = append(data, make([]byte, 512)...)
	for _, offset := range []int{0, len(data) - 512} {
		header := data[offset : offset+512]
		copy(header, zfileMagic[:])
		binary.LittleEndian.PutUint32(header[24:], 96)

		flags := uint64(16 | 2 | 4)
		if offset == 0 {
			flags = 16 | 2 | 1
		}

		binary.LittleEndian.PutUint64(header[32:], flags)
		binary.LittleEndian.PutUint64(header[40:], uint64(indexOffset))
		binary.LittleEndian.PutUint64(header[48:], uint64(len(lengths)))
		binary.LittleEndian.PutUint64(header[56:], uint64(len(payload)))
		binary.LittleEndian.PutUint32(header[64:], indexCRC)
		binary.LittleEndian.PutUint32(header[72:], 4096)

		header[76] = algorithm
		if checksum {
			header[88] = 1
		}

		sealZFileHeader(header)
	}

	return data
}

func sealZFileHeader(header []byte) {
	clear(header[28:32])
	binary.LittleEndian.PutUint32(header[28:32], zfileChecksum(header, 0))
}
