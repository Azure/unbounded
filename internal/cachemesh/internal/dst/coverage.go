// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package dst

import (
	"fmt"
	"slices"
	"strings"
)

// CoverageHandle counts hits of one coverage point. Hit is a single
// increment.
type CoverageHandle struct {
	n *uint64
}

// Hit records that the state was reached.
func (h CoverageHandle) Hit() { *h.n++ }

// Count returns the number of hits.
func (h CoverageHandle) Count() uint64 { return *h.n }

// Coverage records which named states a simulation reached. Registered
// states that are never hit are reported by Missing.
type Coverage struct {
	hits       map[string]*uint64
	registered map[string]bool
}

// NewCoverage returns an empty registry.
func NewCoverage() *Coverage {
	return &Coverage{hits: make(map[string]*uint64), registered: make(map[string]bool)}
}

func (c *Coverage) counter(name string) *uint64 {
	p, ok := c.hits[name]
	if !ok {
		p = new(uint64)
		c.hits[name] = p
	}

	return p
}

// Register declares states that must be reached.
func (c *Coverage) Register(names ...string) {
	for _, n := range names {
		c.registered[n] = true
		c.counter(n)
	}
}

// Handle returns a cheap handle for the named state. Getting a handle does
// not register the state as required.
func (c *Coverage) Handle(name string) CoverageHandle {
	return CoverageHandle{n: c.counter(name)}
}

// Hit records one hit of the named state.
func (c *Coverage) Hit(name string) { *c.counter(name)++ }

// Count returns the hits of the named state.
func (c *Coverage) Count(name string) uint64 {
	if p, ok := c.hits[name]; ok {
		return *p
	}

	return 0
}

// Merge adds o's hits and registrations into c.
func (c *Coverage) Merge(o *Coverage) {
	for k, v := range o.hits {
		*c.counter(k) += *v
	}

	for k := range o.registered {
		c.registered[k] = true
	}
}

// Missing returns the registered states with no hits, sorted.
func (c *Coverage) Missing() []string {
	var out []string

	for k := range c.registered {
		if *c.hits[k] == 0 {
			out = append(out, k)
		}
	}

	slices.Sort(out)

	return out
}

// Report lists every known state with its hit count in name order, marking
// registered states that were missed.
func (c *Coverage) Report() string {
	names := make([]string, 0, len(c.hits))
	for k := range c.hits {
		names = append(names, k)
	}

	slices.Sort(names)

	var b strings.Builder

	for _, k := range names {
		mark := ""
		if c.registered[k] && *c.hits[k] == 0 {
			mark = "  MISSING"
		}

		fmt.Fprintf(&b, "%-40s %10d%s\n", k, *c.hits[k], mark)
	}

	return b.String()
}
