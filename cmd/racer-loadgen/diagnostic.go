// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"github.com/opencontainers/go-digest"
)

const (
	diagnosticPageSize    = 16 << 20
	diagnosticPageRecords = 8
)

var errDiagnosticOracle = errors.New("diagnostic oracle read failed")

type pageRecord struct {
	Index, Offset, Length int64
	Actual, Expected      [sha256.Size]byte
}

type pageEvidence struct {
	firstMismatch         int64
	examined, mismatching int64
	records               []pageRecord
}

// LogValue imposes a second bound at the logging boundary and emits no payload.
func (e *pageEvidence) LogValue() slog.Value {
	pages := make([]any, 0, min(len(e.records), diagnosticPageRecords))
	for _, p := range e.records[:min(len(e.records), diagnosticPageRecords)] {
		pages = append(pages, map[string]any{
			"index": p.Index, "offset": p.Offset, "length": p.Length,
			"actual_digest":   fmt.Sprintf("sha256:%x", p.Actual),
			"expected_digest": fmt.Sprintf("sha256:%x", p.Expected),
		})
	}

	return slog.GroupValue(slog.Int64("first_mismatch_offset", e.firstMismatch),
		slog.Int("page_size", diagnosticPageSize), slog.Int64("pages_examined", e.examined),
		slog.Int64("mismatching_pages", e.mismatching),
		slog.Int64("omitted_mismatching_pages", max(0, e.mismatching-int64(len(pages)))), slog.Any("records", pages))
}

func (p *puller) configureDiagnostics(catalog *imageCatalog) error {
	if !p.opts.DiagnoseIntegrity {
		return nil
	}

	if catalog == nil || len(catalog.images) == 0 {
		return errors.New("diagnostic catalog is empty")
	}

	p.expected = make(map[digest.Digest]imageBlob, len(catalog.blobs)+len(catalog.images))
	for key, blob := range catalog.blobs {
		if blob.data == nil || blob.descriptor.Digest != key || blob.descriptor.Size < 0 {
			return errors.New("invalid diagnostic blob")
		}

		p.expected[key] = blob
	}

	for _, img := range catalog.images {
		if img == nil || img.Manifest.Size != int64(len(img.manifest)) || img.Manifest.Digest != digest.FromBytes(img.manifest) {
			return errors.New("invalid diagnostic manifest")
		}

		p.expected[img.Manifest.Digest] = imageBlob{descriptor: img.Manifest, data: bytes.NewReader(img.manifest)}
	}

	return nil
}

func (p *puller) readBodyDiagnostic(body io.Reader, expected imageBlob) (int64, string, *pageEvidence, error) {
	buffer, ok := p.buffers.Get().(*[]byte)
	if !ok {
		return 0, "", nil, errors.New("invalid pull buffer")
	}
	defer p.buffers.Put(buffer)

	oracle := make([]byte, len(*buffer))
	whole, actualPage, expectedPage := sha256.New(), sha256.New(), sha256.New()
	evidence := &pageEvidence{firstMismatch: -1}

	var (
		total, pageBytes int64
		pageMismatch     bool
	)

	finish := func() {
		if pageMismatch {
			evidence.mismatching++
			if len(evidence.records) < diagnosticPageRecords {
				r := pageRecord{Index: evidence.examined, Offset: evidence.examined * diagnosticPageSize, Length: pageBytes}
				copy(r.Actual[:], actualPage.Sum(nil))
				copy(r.Expected[:], expectedPage.Sum(nil))
				evidence.records = append(evidence.records, r)
			}
		}

		evidence.examined++

		actualPage.Reset()
		expectedPage.Reset()

		pageBytes, pageMismatch = 0, false
	}

	for {
		n, err := body.Read(*buffer)
		if n > 0 {
			data := (*buffer)[:n]
			p.metrics.receivedBytes.Add(float64(n))

			_, _ = whole.Write(data)

			comparable := min(int64(n), max(0, expected.descriptor.Size-total))
			if comparable > 0 {
				got, readErr := expected.data.ReadAt(oracle[:comparable], total)
				if int64(got) != comparable || (readErr != nil && !errors.Is(readErr, io.EOF)) {
					return total + int64(n), "", nil, errDiagnosticOracle
				}
			}

			for offset := int64(0); offset < comparable; {
				length := min(comparable-offset, diagnosticPageSize-pageBytes)

				actual, want := data[offset:offset+length], oracle[offset:offset+length]
				if !bytes.Equal(actual, want) {
					pageMismatch = true

					if evidence.firstMismatch < 0 {
						for i := range actual {
							if actual[i] != want[i] {
								evidence.firstMismatch = total + offset + int64(i)
								break
							}
						}
					}
				}

				_, _ = actualPage.Write(actual)
				_, _ = expectedPage.Write(want)
				pageBytes += length
				offset += length

				if pageBytes == diagnosticPageSize {
					finish()
				}
			}

			total += int64(n)
		}

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return total, "", nil, err
		}
	}

	if total == expected.descriptor.Size && pageBytes > 0 {
		finish()
	}

	return total, fmt.Sprintf("sha256:%x", whole.Sum(nil)), evidence, nil
}
