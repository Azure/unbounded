// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"
)

const (
	sectorSize        = int64(512)
	headerSize        = int64(4096)
	mappingSize       = int64(16)
	maxROIndexEntries = uint64(1_000_000)
	maxStackLayers    = 255
	invalidOffset     = uint64(1<<50 - 1)
)

var (
	magic0 = [8]byte{'L', 'S', 'M', 'T', 0, 1, 2, 0}
	magic1 = [16]byte{0x65, 0x7e, 0x63, 0xd2, 0x94, 0x44, 0x08, 0x4c, 0xa2, 0xd2, 0xc8, 0xec, 0x4f, 0xcf, 0xae, 0x8a}
)

// Mapping maps logical sectors to sectors in one sealed LSMT blob.
type Mapping struct {
	Offset  uint64
	Length  uint32
	MOffset uint64
	Zeroed  bool
}

func (m Mapping) end() uint64 { return m.Offset + uint64(m.Length) }

// LSMTLayer is one validated read-only OverlayBD v0.1.0 layer.
type LSMTLayer struct {
	reader      io.ReaderAt
	virtualSize int64
	mappings    []Mapping
}

// OpenLSMTLayer validates and opens one complete ACR-produced OverlayBD layer.
func OpenLSMTLayer(reader io.ReaderAt, size int64) (*LSMTLayer, error) {
	if reader == nil || size < 512 {
		return nil, fmt.Errorf("overlaybd LSMT: invalid layer size %d", size)
	}

	reader, size, err := openLayerPayload(reader, size)
	if err != nil {
		return nil, err
	}

	if size < 2*headerSize {
		return nil, fmt.Errorf("overlaybd LSMT: invalid payload size %d", size)
	}

	header := make([]byte, headerSize)
	if err := readFullAt(reader, header, 0); err != nil {
		return nil, fmt.Errorf("overlaybd LSMT: read header: %w", err)
	}

	if err := validateHeaderTrailer(header, true); err != nil {
		return nil, fmt.Errorf("overlaybd LSMT: header: %w", err)
	}

	if indexCount := binary.LittleEndian.Uint64(header[40:48]); indexCount > maxROIndexEntries {
		return nil, fmt.Errorf("overlaybd LSMT: header index entries %d exceed %d", indexCount, maxROIndexEntries)
	}

	trailer := make([]byte, headerSize)
	if err := readFullAt(reader, trailer, size-headerSize); err != nil {
		return nil, fmt.Errorf("overlaybd LSMT: read trailer: %w", err)
	}

	if err := validateHeaderTrailer(trailer, false); err != nil {
		return nil, fmt.Errorf("overlaybd LSMT: trailer: %w", err)
	}

	indexOffset := binary.LittleEndian.Uint64(trailer[32:40])
	indexCount := binary.LittleEndian.Uint64(trailer[40:48])
	virtualSize := binary.LittleEndian.Uint64(trailer[48:56])

	if indexCount > maxROIndexEntries {
		return nil, fmt.Errorf("overlaybd LSMT: index entries %d exceed %d", indexCount, maxROIndexEntries)
	}

	if virtualSize > uint64(^uint64(0)>>1) || virtualSize%uint64(sectorSize) != 0 {
		return nil, fmt.Errorf("overlaybd LSMT: invalid virtual size %d", virtualSize)
	}

	indexBytes := indexCount * uint64(mappingSize)

	trailerOffset := uint64(size - headerSize)
	if indexOffset < uint64(headerSize) || indexOffset > trailerOffset || indexBytes > trailerOffset-indexOffset {
		return nil, fmt.Errorf("overlaybd LSMT: invalid index range %d+%d", indexOffset, indexBytes)
	}

	rawIndex := make([]byte, indexBytes)
	if err := readFullAt(reader, rawIndex, int64(indexOffset)); err != nil {
		return nil, fmt.Errorf("overlaybd LSMT: read index: %w", err)
	}

	mappings := make([]Mapping, 0, indexCount)
	physicalEnd := indexOffset / uint64(sectorSize)
	virtualEnd := virtualSize / uint64(sectorSize)

	for offset := uint64(0); offset < indexBytes; offset += uint64(mappingSize) {
		mapping, err := decodeMapping(rawIndex[offset : offset+uint64(mappingSize)])
		if err != nil {
			return nil, fmt.Errorf("overlaybd LSMT: mapping %d: %w", offset/uint64(mappingSize), err)
		}

		if mapping.Offset == invalidOffset {
			continue
		}

		if mapping.Length == 0 || virtualEnd != 0 && mapping.end() > virtualEnd {
			return nil, fmt.Errorf("overlaybd LSMT: invalid logical mapping %+v", mapping)
		}

		if !mapping.Zeroed && (mapping.MOffset < uint64(headerSize/sectorSize) || mapping.MOffset+uint64(mapping.Length) > physicalEnd) {
			return nil, fmt.Errorf("overlaybd LSMT: invalid physical mapping %+v", mapping)
		}

		if len(mappings) != 0 && mappings[len(mappings)-1].end() > mapping.Offset {
			return nil, fmt.Errorf("overlaybd LSMT: mappings overlap or are unsorted")
		}

		mappings = append(mappings, mapping)
	}

	return &LSMTLayer{reader: reader, virtualSize: int64(virtualSize), mappings: mappings}, nil
}

func validateHeaderTrailer(data []byte, header bool) error {
	if len(data) != int(headerSize) || string(data[:8]) != string(magic0[:]) || string(data[8:24]) != string(magic1[:]) {
		return errors.New("invalid magic")
	}

	if binary.LittleEndian.Uint32(data[24:28]) != 390 {
		return errors.New("invalid header size")
	}

	flags := binary.LittleEndian.Uint32(data[28:32])
	if header {
		if flags&1 == 0 {
			return errors.New("header flag is not set")
		}
	} else if flags&1 != 0 || flags&2 == 0 || flags&4 == 0 {
		return errors.New("trailer is not a sealed data file")
	}

	if data[132] != 1 {
		return fmt.Errorf("unsupported version %d", data[132])
	}

	return nil
}

func decodeMapping(data []byte) (Mapping, error) {
	logical := binary.LittleEndian.Uint64(data[:8])
	physical := binary.LittleEndian.Uint64(data[8:16])

	mapping := Mapping{
		Offset:  logical & (1<<50 - 1),
		Length:  uint32(logical >> 50),
		MOffset: physical & (1<<55 - 1),
		Zeroed:  physical&(1<<55) != 0,
	}
	if tag := uint8(physical >> 56); tag != 0 {
		return Mapping{}, fmt.Errorf("on-disk tag is %d, want 0", tag)
	}

	return mapping, nil
}

func (l *LSMTLayer) mapping(sector uint64) (Mapping, bool) {
	index := sort.Search(len(l.mappings), func(i int) bool { return l.mappings[i].end() > sector })
	if index == len(l.mappings) || l.mappings[index].Offset > sector {
		return Mapping{}, false
	}

	return l.mappings[index], true
}

// LSMTImage overlays ACR layers in manifest order. Later layers have higher
// precedence, matching OverlayBD's open_files_ro behavior.
type LSMTImage struct {
	layers      []*LSMTLayer
	virtualSize int64
}

func NewLSMTImage(layers ...*LSMTLayer) (*LSMTImage, error) {
	if len(layers) == 0 {
		return nil, errors.New("overlaybd LSMT: no layers")
	}

	if len(layers) > maxStackLayers {
		return nil, fmt.Errorf("overlaybd LSMT: %d layers exceed %d", len(layers), maxStackLayers)
	}

	var virtualSize int64

	for _, layer := range layers {
		if layer == nil {
			return nil, errors.New("overlaybd LSMT: nil layer")
		}

		if layer.virtualSize != 0 {
			virtualSize = layer.virtualSize
		}
	}

	if virtualSize == 0 {
		return nil, errors.New("overlaybd LSMT: no virtual size")
	}

	return &LSMTImage{layers: append([]*LSMTLayer(nil), layers...), virtualSize: virtualSize}, nil
}

func (i *LSMTImage) Size() int64 { return i.virtualSize }

// ReadAt reads the merged virtual disk. Holes and zero mappings read as zero.
func (i *LSMTImage) ReadAt(p []byte, offset int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	if offset < 0 {
		return 0, fmt.Errorf("overlaybd LSMT: negative offset %d", offset)
	}

	if offset >= i.virtualSize {
		return 0, io.EOF
	}

	want := int64(len(p))
	short := false

	if remaining := i.virtualSize - offset; want > remaining {
		want = remaining
		short = true
	}

	clear(p[:want])

	lastSector := (offset + want - 1) / sectorSize
	for sector := offset / sectorSize; sector <= lastSector; {
		resolved := i.resolveSector(uint64(sector))

		runEnd := sector + 1
		for runEnd <= lastSector {
			next := i.resolveSector(uint64(runEnd))
			if resolved.data != next.data || resolved.layer != next.layer || resolved.data && next.physical != resolved.physical+uint64(runEnd-sector) {
				break
			}

			runEnd++
		}

		virtualStart := max(offset, sector*sectorSize)

		virtualEnd := min(offset+want, runEnd*sectorSize)
		if resolved.data {
			physicalStart := int64(resolved.physical)*sectorSize + virtualStart - sector*sectorSize
			if err := readFullAt(i.layers[resolved.layer].reader, p[virtualStart-offset:virtualEnd-offset], physicalStart); err != nil {
				return int(virtualStart - offset), fmt.Errorf("overlaybd LSMT: read layer %d sector %d: %w", resolved.layer, sector, err)
			}
		}

		sector = runEnd
	}

	if short {
		return int(want), io.EOF
	}

	return int(want), nil
}

type resolvedSector struct {
	layer    int
	physical uint64
	data     bool
}

func (i *LSMTImage) resolveSector(sector uint64) resolvedSector {
	for layerIndex := len(i.layers) - 1; layerIndex >= 0; layerIndex-- {
		layer := i.layers[layerIndex]

		mapping, ok := layer.mapping(sector)
		if !ok {
			continue
		}

		if mapping.Zeroed {
			return resolvedSector{layer: -1}
		}

		return resolvedSector{layer: layerIndex, physical: mapping.MOffset + sector - mapping.Offset, data: true}
	}

	return resolvedSector{layer: -1}
}

func readFullAt(reader io.ReaderAt, buffer []byte, offset int64) error {
	n, err := reader.ReadAt(buffer, offset)
	if n != len(buffer) {
		if err == nil {
			err = io.ErrUnexpectedEOF
		}

		return err
	}

	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}

	return nil
}
