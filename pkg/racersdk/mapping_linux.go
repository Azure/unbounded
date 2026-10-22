// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// mappedSupported reports whether this platform requests mapped delivery.
const mappedSupported = true

// rightsSpace sizes the control buffer for a few descriptors, so unexpected
// extras are received and closed rather than truncated.
var rightsSpace = syscall.CmsgSpace(4 * 4)

// rightsReader reads a Unix socket and collects descriptors passed with
// SCM_RIGHTS. Go's ReadMsgUnix sets MSG_CMSG_CLOEXEC on Linux.
type rightsReader struct {
	conn *net.UnixConn
	oob  []byte

	mu  sync.Mutex
	fds []int
	err error // a malformed or truncated control message
}

func (r *rightsReader) Read(b []byte) (int, error) {
	n, oobn, flags, _, err := r.conn.ReadMsgUnix(b, r.oob)
	if oobn > 0 || flags&unix.MSG_CTRUNC != 0 {
		r.collect(r.oob[:max(oobn, 0)], flags&unix.MSG_CTRUNC != 0)
	}

	// ReadMsgUnix reports -1 with some errors; io.Reader must not.
	return max(n, 0), err
}

func (r *rightsReader) collect(oob []byte, truncated bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if truncated {
		r.err = errors.New("truncated control message")
	}

	messages, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		r.err = err
		return
	}

	for i := range messages {
		fds, err := syscall.ParseUnixRights(&messages[i])
		if err != nil {
			// Not SCM_RIGHTS, such as credentials; nothing to close.
			continue
		}

		r.fds = append(r.fds, fds...)
	}
}

// take returns and forgets every descriptor received so far, and any control
// message error.
func (r *rightsReader) take() ([]int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	fds, err := r.fds, r.err
	r.fds, r.err = nil, nil

	return fds, err
}

// receiveRights makes conn's buffered reader collect passed descriptors. It
// must be called before anything is read from conn. Other connection types,
// such as pipes in tests, cannot carry descriptors and are left alone.
func receiveRights(conn *clientConn) {
	raw, ok := conn.Conn.(*net.UnixConn)
	if !ok {
		return
	}

	conn.rights = &rightsReader{conn: raw, oob: make([]byte, rightsSpace)}
	conn.r.Reset(conn.rights)
}

func closeFDs(fds []int) {
	for _, fd := range fds {
		_ = unix.Close(fd) //nolint:errcheck // Discarded descriptor.
	}
}

// mapping is a read-only shared memory region received from Racer.
type mapping struct {
	data     []byte
	dev, ino uint64
	refs     int
}

// mappings caches the current mapping so successive reads reuse it instead of
// mapping the same memory again.
type mappings struct {
	mu      sync.Mutex
	current *mapping
	closed  bool

	// Test hooks.
	maps, unmaps int
}

// acquire maps fd, or takes a reference to the current mapping when fd names
// the same memory. It always closes fd.
func (m *mappings) acquire(fd int) (*mapping, error) {
	defer closeFDs([]int{fd})

	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("stat delivery memory: %w", err)
	}

	if st.Size <= 0 || uint64(st.Size) > uint64(^uint(0)>>1) {
		return nil, errors.New("invalid delivery memory size")
	}

	dev, ino := uint64(st.Dev), st.Ino //nolint:unconvert // Dev is not uint64 on every Linux architecture.

	m.mu.Lock()
	defer m.mu.Unlock()

	// The same memory may have grown; map it again to cover the new size.
	if c := m.current; c != nil && c.dev == dev && c.ino == ino && int64(len(c.data)) >= st.Size {
		c.refs++
		return c, nil
	}

	data, err := unix.Mmap(fd, 0, int(st.Size), unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("map delivery memory: %w", err)
	}

	m.maps++

	next := &mapping{data: data, dev: dev, ino: ino, refs: 1}

	if !m.closed {
		old := m.current
		m.current = next

		if old != nil && old.refs == 0 {
			m.unmapLocked(old)
		}
	}

	return next, nil
}

// release drops a reference and unmaps the region once it is unreferenced
// and no longer current.
func (m *mappings) release(mp *mapping) {
	if mp == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	mp.refs--
	if mp.refs == 0 && mp != m.current {
		m.unmapLocked(mp)
	}
}

// close stops caching and unmaps the current region once it is unreferenced.
func (m *mappings) close() {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true

	if c := m.current; c != nil {
		m.current = nil

		if c.refs == 0 {
			m.unmapLocked(c)
		}
	}
}

func (m *mappings) unmapLocked(mp *mapping) {
	if mp.data == nil {
		return
	}

	_ = unix.Munmap(mp.data) //nolint:errcheck // Nothing useful to do on failure.
	mp.data = nil
	m.unmaps++
}

// counts reports how many regions were mapped and unmapped, for tests.
func (m *mappings) counts() (int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.maps, m.unmaps
}
