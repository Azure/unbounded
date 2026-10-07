// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package racersdk

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestOriginSocketPublicationMode(t *testing.T) {
	for name, listen := range map[string]func(string, os.FileMode) (*net.UnixListener, func(), error){
		"default": listenOrigin,
		"owned":   listenOwnedOrigin,
		"witness": listenOriginAtWitness,
	} {
		t.Run(name, func(t *testing.T) {
			dir := ownedSocketTestDir(t)
			require.NoError(t, os.Chmod(dir, 0o755))

			fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
			require.NoError(t, err)

			defer unix.Close(fd)

			_, err = unix.InotifyAddWatch(fd, dir, unix.IN_CREATE|unix.IN_ATTRIB)
			require.NoError(t, err)

			path := filepath.Join(dir, "socket")
			_, cleanup, err := listen(path, 0o600)
			require.NoError(t, err)

			defer cleanup()

			info, err := os.Lstat(path)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

			conn, err := net.DialTimeout("unix", path, time.Second)
			require.NoError(t, err)
			closeQuietly(conn)

			// Directory events retain ordering even if the entire bind completed
			// before this read. A chmod on either public name exposes the window.
			buffer := make([]byte, 8192)
			n, err := unix.Read(fd, buffer)
			require.NoError(t, err)

			created := false

			for offset := 0; offset < n; {
				mask := binary.NativeEndian.Uint32(buffer[offset+4:])
				length := int(binary.NativeEndian.Uint32(buffer[offset+12:]))

				entry := strings.TrimRight(string(buffer[offset+unix.SizeofInotifyEvent:offset+unix.SizeofInotifyEvent+length]), "\x00")
				if entry == "socket" || entry == ".racer-origin.socket" {
					require.Zero(t, mask&unix.IN_ATTRIB, "public socket mode changed after publication: %s", entry)
					created = created || entry == "socket" && mask&unix.IN_CREATE != 0
				}

				offset += unix.SizeofInotifyEvent + length
			}

			require.True(t, created, "socket creation event missing")
		})
	}
}

func TestOriginSocketPublicationDoesNotOverwrite(t *testing.T) {
	for _, kind := range []string{"file", "symlink", "socket"} {
		t.Run(kind, func(t *testing.T) {
			dir := ownedSocketTestDir(t)
			path := filepath.Join(dir, "socket")

			switch kind {
			case "file":
				require.NoError(t, os.WriteFile(path, []byte("keep"), 0o600))
			case "symlink":
				require.NoError(t, os.Symlink("missing", path))
			case "socket":
				listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
				require.NoError(t, err)

				defer closeQuietly(listener)
			}

			before, err := os.Lstat(path)
			require.NoError(t, err)

			listener, err := bindOriginSocket(path, 0o640)
			require.ErrorIs(t, err, os.ErrExist)
			require.Nil(t, listener)

			after, err := os.Lstat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after), "replaced existing entry")
			require.Equal(t, before.Mode(), after.Mode())

			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			require.Len(t, entries, 1, "failed publication leaked staging files")
		})
	}
}

func TestOriginSocketPublicationConcurrent(t *testing.T) {
	dir := ownedSocketTestDir(t)
	path := filepath.Join(dir, "socket")
	start := make(chan struct{})
	listeners := make(chan *net.UnixListener, 8)
	failures := make(chan error, 8)

	var group sync.WaitGroup

	for range cap(listeners) {
		group.Go(func() {
			<-start

			listener, err := bindOriginSocket(path, 0o600)
			if err != nil {
				failures <- err
				return
			}

			listeners <- listener
		})
	}

	close(start)
	group.Wait()
	close(listeners)
	close(failures)

	for listener := range listeners {
		t.Cleanup(func() { closeQuietly(listener) })
	}

	require.Len(t, failures, 7, "publication must have exactly one winner")

	for err := range failures {
		require.ErrorIs(t, err, os.ErrExist)
	}

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "publication leaked staging files")

	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err)
	closeQuietly(conn)
}

func TestOriginSocketPublicationPathLimit(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o640, 0o660} {
		dir := ownedSocketTestDir(t)
		path := filepath.Join(dir, strings.Repeat("s", socketPathLimit-len(dir)-1))
		require.Len(t, path, socketPathLimit)

		_, cleanup, err := listenOrigin(path, mode)
		require.NoError(t, err)

		defer cleanup()

		info, err := os.Lstat(path)
		require.NoError(t, err)
		require.Equal(t, mode, info.Mode().Perm())

		conn, err := net.DialTimeout("unix", path, time.Second)
		require.NoError(t, err)
		closeQuietly(conn)
		cleanup()

		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Empty(t, entries, "cleanup retained socket or staging files")
	}
}
