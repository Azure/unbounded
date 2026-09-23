// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

// Configuration is shared by all frontends and origins of a cache. Names are
// immutable, including after deletion/recreation. Credentials never enter keys.
type configuration struct {
	Endpoint string       `json:"azure_endpoint"`
	Objects  []objectSpec `json:"objects"`
}

type objectSpec struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	Container string `json:"container"`
	Blob      string `json:"blob"`
	target    string
	etag      string
}

func loadConfiguration(path string) (*configuration, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer closeResource(f)

	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.DisallowUnknownFields()

	var c configuration
	if err := d.Decode(&c); err != nil {
		return nil, err
	}

	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("configuration must contain one JSON object")
	}

	if err := c.validate(); err != nil {
		return nil, err
	}

	return &c, nil
}

func (c *configuration) validate() error {
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("azure_endpoint must be an HTTP(S) service URL without credentials or query")
	}

	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	if len(c.Objects) == 0 {
		return fmt.Errorf("objects must not be empty")
	}

	seen := make(map[string]bool)

	for i := range c.Objects {
		o := &c.Objects[i]
		if o.Bucket == "" || strings.ContainsAny(o.Bucket, "/?#\\") || o.Key == "" || o.Container == "" || strings.ContainsAny(o.Container, "/?#\\") || o.Blob == "" {
			return fmt.Errorf("object %d: bucket, key, container and blob are required", i)
		}

		path := "/" + o.Bucket + "/" + o.Key
		if seen[path] {
			return fmt.Errorf("duplicate S3 object %q", path)
		}

		seen[path] = true
		// JSON provides unambiguous field boundaries and a versioned domain.
		identity, err := json.Marshal([]string{"racer-object/azure/v1", c.Endpoint, o.Container, o.Blob})
		if err != nil {
			return err
		}

		hash := fmt.Sprintf("%x", sha256.Sum256(identity))
		o.target = "/racer-object/v1/" + hash
		o.etag = `"` + hash + `"`
	}

	return nil
}
