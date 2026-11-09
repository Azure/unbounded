// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package overlaybd

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
)

func openLayerPayload(reader io.ReaderAt, size int64) (io.ReaderAt, int64, error) {
	if size < 512 {
		return nil, 0, fmt.Errorf("overlaybd payload: invalid size %d", size)
	}

	header := make([]byte, 512)
	if err := readFullAt(reader, header, 0); err != nil {
		return nil, 0, fmt.Errorf("overlaybd payload: read header: %w", err)
	}

	if bytes.Equal(header[:8], magic0[:]) {
		return reader, size, nil
	}

	if bytes.Equal(header[257:262], []byte("ustar")) {
		section := io.NewSectionReader(reader, 0, size)
		archive := tar.NewReader(io.LimitReader(section, 1<<20))

		entry, err := archive.Next()
		if err != nil {
			return nil, 0, fmt.Errorf("overlaybd payload: tar header: %w", err)
		}

		if entry.Typeflag != tar.TypeReg || entry.Name != "overlaybd.commit" {
			return nil, 0, fmt.Errorf("overlaybd payload: unexpected tar entry %q type %d", entry.Name, entry.Typeflag)
		}

		offset, err := section.Seek(0, io.SeekCurrent)
		if err != nil {
			return nil, 0, err
		}

		if entry.Size < 512 || offset > size || entry.Size > size-offset {
			return nil, 0, fmt.Errorf("overlaybd payload: tar entry range %d+%d exceeds %d", offset, entry.Size, size)
		}

		reader = io.NewSectionReader(reader, offset, entry.Size)
		size = entry.Size

		if err := readFullAt(reader, header, 0); err != nil {
			return nil, 0, fmt.Errorf("overlaybd payload: read tar payload: %w", err)
		}
	}

	if bytes.Equal(header[:8], zfileMagic[:8]) {
		compressed, err := openZFile(reader, size)
		if err != nil {
			return nil, 0, err
		}

		return compressed, compressed.size, nil
	}

	return reader, size, nil
}
