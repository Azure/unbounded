// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storagesupervisor

import (
	"fmt"
	"net/url"
	"strings"
)

type annotationListEntry struct {
	Item   string
	Values url.Values
}

func parseAnnotationList(raw string) ([]annotationListEntry, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}

	parts := strings.Split(raw, ",")
	entries := make([]annotationListEntry, 0, len(parts))

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		item, query, hasQuery := strings.Cut(part, "?")
		item = strings.TrimSpace(item)

		if item == "" {
			return nil, fmt.Errorf("empty annotation list item in %q", part)
		}

		values := url.Values{}

		if hasQuery {
			parsed, err := url.ParseQuery(query)
			if err != nil {
				return nil, fmt.Errorf("parse query for %q: %w", item, err)
			}

			for key := range parsed {
				if strings.TrimSpace(key) == "" {
					return nil, fmt.Errorf("empty query key for %q", item)
				}
			}

			values = parsed
		}

		entries = append(entries, annotationListEntry{Item: item, Values: values})
	}

	return entries, nil
}
