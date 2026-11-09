// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

const (
	zfileHeaderSize = 512
	maxZFileBlocks  = 4 << 20
	zfileReadBlocks = 64
)

var (
	zfileMagic = [24]byte{'Z', 'F', 'i', 'l', 'e', 0, 1, 0, 't', 'u', 'j', 'i', '.', 'y', 'y', 'f', '@', 'A', 'l', 'i', 'b', 'a', 'b', 'a'}
	zfileCRC32 = crc32.MakeTable(crc32.Castagnoli)
)

type zfileReader struct {
	reader    io.ReaderAt
	size      int64
	blockSize int64
	offsets   []int64
	algorithm byte
	checksum  bool
}

func openZFile(reader io.ReaderAt, size int64) (*zfileReader, error) {
	if size < 2*zfileHeaderSize {
		return nil, fmt.Errorf("overlaybd ZFile: invalid size %d", size)
	}

	header := make([]byte, zfileHeaderSize)
	trailer := make([]byte, zfileHeaderSize)

	if err := readFullAt(reader, header, 0); err != nil {
		return nil, fmt.Errorf("overlaybd ZFile: read header: %w", err)
	}

	if err := validateZFileHeader(header, true); err != nil {
		return nil, err
	}

	if err := readFullAt(reader, trailer, size-zfileHeaderSize); err != nil {
		return nil, fmt.Errorf("overlaybd ZFile: read trailer: %w", err)
	}

	if err := validateZFileHeader(trailer, false); err != nil {
		return nil, err
	}

	if !bytes.Equal(header[72:96], trailer[72:96]) {
		return nil, fmt.Errorf("overlaybd ZFile: compression options changed between header and trailer")
	}

	blockSize := uint64(binary.LittleEndian.Uint32(trailer[72:76]))
	algorithm := trailer[76]

	if blockSize < 512 || blockSize > 65536 || blockSize&(blockSize-1) != 0 {
		return nil, fmt.Errorf("overlaybd ZFile: unsupported block size %d", blockSize)
	}

	if algorithm != 1 && algorithm != 2 {
		return nil, fmt.Errorf("overlaybd ZFile: unsupported compression algorithm %d", algorithm)
	}

	if trailer[78] != 0 || binary.LittleEndian.Uint32(trailer[84:88]) != 0 {
		return nil, fmt.Errorf("overlaybd ZFile: compression dictionaries are unsupported")
	}

	if trailer[88] > 1 {
		return nil, fmt.Errorf("overlaybd ZFile: invalid block checksum option %d", trailer[88])
	}

	indexOffset := binary.LittleEndian.Uint64(trailer[40:48])
	blockCount := binary.LittleEndian.Uint64(trailer[48:56])

	originalSize := binary.LittleEndian.Uint64(trailer[56:64])
	if blockCount == 0 || blockCount > maxZFileBlocks || originalSize == 0 || originalSize > blockCount*blockSize || originalSize <= (blockCount-1)*blockSize {
		return nil, fmt.Errorf("overlaybd ZFile: invalid original size %d or block count %d", originalSize, blockCount)
	}

	end := uint64(size - zfileHeaderSize)

	indexBytes := blockCount * 4
	if indexOffset < zfileHeaderSize || indexOffset > end || indexBytes > end-indexOffset {
		return nil, fmt.Errorf("overlaybd ZFile: invalid index range %d+%d", indexOffset, indexBytes)
	}

	index := make([]byte, indexBytes)
	if err := readFullAt(reader, index, int64(indexOffset)); err != nil {
		return nil, fmt.Errorf("overlaybd ZFile: read index: %w", err)
	}

	if binary.LittleEndian.Uint64(trailer[32:40])&16 != 0 && zfileChecksum(index, 0) != binary.LittleEndian.Uint32(trailer[64:68]) {
		return nil, fmt.Errorf("overlaybd ZFile: index checksum mismatch")
	}

	checksum := trailer[88] == 1
	offsets := make([]int64, blockCount+1)
	offsets[0] = zfileHeaderSize

	for block := uint64(0); block < blockCount; block++ {
		length := uint64(binary.LittleEndian.Uint32(index[block*4:]))

		minLength := uint64(0)
		if checksum {
			minLength = 4
		}

		if length <= minLength || length > 2*blockSize+4 || uint64(offsets[block]) > indexOffset || length > indexOffset-uint64(offsets[block]) {
			return nil, fmt.Errorf("overlaybd ZFile: invalid compressed block %d length %d", block, length)
		}

		offsets[block+1] = offsets[block] + int64(length)
	}

	if uint64(offsets[blockCount]) != indexOffset {
		return nil, fmt.Errorf("overlaybd ZFile: compressed blocks do not end at the index")
	}

	return &zfileReader{
		reader: reader, size: int64(originalSize), blockSize: int64(blockSize),
		offsets: offsets, algorithm: algorithm, checksum: checksum,
	}, nil
}

func validateZFileHeader(data []byte, header bool) error {
	if !bytes.Equal(data[:24], zfileMagic[:]) || binary.LittleEndian.Uint32(data[24:28]) != 96 {
		return fmt.Errorf("overlaybd ZFile: invalid header/trailer magic or size")
	}

	flags := binary.LittleEndian.Uint64(data[32:40])
	if flags&^uint64(31) != 0 || flags&2 == 0 || (flags&1 != 0) != header || !header && flags&4 == 0 {
		return fmt.Errorf("overlaybd ZFile: unsupported header/trailer flags %#x", flags)
	}

	if flags&16 != 0 {
		expected := binary.LittleEndian.Uint32(data[28:32])
		copyOfHeader := bytes.Clone(data)
		clear(copyOfHeader[28:32])

		if zfileChecksum(copyOfHeader, 0) != expected {
			return fmt.Errorf("overlaybd ZFile: header/trailer checksum mismatch")
		}
	}

	return nil
}

func zfileChecksum(data []byte, seed uint32) uint32 {
	// ZFile stores the uncomplemented CRC32C accumulator.
	return ^crc32.Update(^seed, zfileCRC32, data)
}

func (r *zfileReader) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if offset < 0 {
		return 0, fmt.Errorf("overlaybd ZFile: negative read offset %d", offset)
	}

	if offset >= r.size {
		return 0, io.EOF
	}

	want := int64(len(p))
	if want > r.size-offset {
		want = r.size - offset
	}

	first := offset / r.blockSize
	last := (offset + want - 1) / r.blockSize
	copied := 0

	for batch := first; batch <= last; {
		batchEnd := min(last+1, batch+zfileReadBlocks)

		compressed := make([]byte, r.offsets[batchEnd]-r.offsets[batch])
		if err := readFullAt(r.reader, compressed, r.offsets[batch]); err != nil {
			return copied, fmt.Errorf("overlaybd ZFile: read compressed blocks: %w", err)
		}

		for block := batch; block < batchEnd; block++ {
			start := r.offsets[block] - r.offsets[batch]
			end := r.offsets[block+1] - r.offsets[batch]

			decoded, err := r.decodeBlock(compressed[start:end], block)
			if err != nil {
				return copied, err
			}

			begin := max(offset-block*r.blockSize, 0)
			count := min(int64(len(decoded))-begin, want-int64(copied))
			copied += copy(p[copied:], decoded[begin:begin+count])
		}

		batch = batchEnd
	}

	if copied < len(p) {
		return copied, io.EOF
	}

	return copied, nil
}

func (r *zfileReader) decodeBlock(compressed []byte, block int64) ([]byte, error) {
	if r.checksum {
		checksumOffset := len(compressed) - 4
		expected := binary.LittleEndian.Uint32(compressed[checksumOffset:])

		compressed = compressed[:checksumOffset]
		if zfileChecksum(compressed, 100007) != expected {
			return nil, fmt.Errorf("overlaybd ZFile: block %d checksum mismatch", block)
		}
	}

	expectedSize := min(r.blockSize, r.size-block*r.blockSize)

	decoded := make([]byte, expectedSize)
	if r.algorithm == 1 {
		n, err := lz4.UncompressBlock(compressed, decoded)
		if err != nil || int64(n) != expectedSize {
			return nil, fmt.Errorf("overlaybd ZFile: LZ4 block %d decoded %d bytes, want %d: %v", block, n, expectedSize, err)
		}

		return decoded, nil
	}

	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(8<<20), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, fmt.Errorf("overlaybd ZFile: initialize Zstandard decoder: %w", err)
	}
	defer decoder.Close()

	decoded, err = decoder.DecodeAll(compressed, decoded[:0])
	if err != nil || int64(len(decoded)) != expectedSize {
		return nil, fmt.Errorf("overlaybd ZFile: Zstandard block %d decoded %d bytes, want %d: %v", block, len(decoded), expectedSize, err)
	}

	return decoded, nil
}
