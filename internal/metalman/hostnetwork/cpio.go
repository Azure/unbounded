// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnetwork

import (
	"bufio"
	"fmt"
	"io"
	"io/fs"
	"strings"
)

// CPIOFile is one entry of a newc archive. Mode carries the permission bits
// and, for directories, fs.ModeDir. Only regular files and directories are
// supported.
type CPIOFile struct {
	Path string
	Mode fs.FileMode
	Data []byte
}

const (
	cpioMagic   = "070701"
	cpioTrailer = "TRAILER!!!"
	cpioDirMode = 0o755
	sIFREG      = 0o100000
	sIFDIR      = 0o040000
	cpioMaxSize = 1<<32 - 1
)

// WriteCPIO writes a deterministic newc ("070701") archive of files. Every
// entry has uid/gid 0, mtime 0, nlink 1 (2 for directories), and sequential
// inode numbers starting at 1. Parent directories that are not listed are
// synthesized with mode 0755 and emitted once, before their first child.
// Paths must be relative, slash-separated, and free of empty, "." and ".."
// components; duplicates are rejected.
func WriteCPIO(w io.Writer, files []CPIOFile) error {
	type entry struct {
		path string
		mode uint32
		data []byte
	}

	var (
		entries []entry
		seen    = map[string]bool{}
	)

	for i, f := range files {
		if err := validCPIOPath(f.Path); err != nil {
			return fmt.Errorf("cpio entry %d: %w", i, err)
		}

		if f.Mode&^(fs.ModeDir|fs.ModePerm) != 0 {
			return fmt.Errorf("cpio entry %q: unsupported mode %v", f.Path, f.Mode)
		}

		if f.Mode.IsDir() && len(f.Data) != 0 {
			return fmt.Errorf("cpio entry %q: directory with data", f.Path)
		}

		if uint64(len(f.Data)) > cpioMaxSize {
			return fmt.Errorf("cpio entry %q: file too large", f.Path)
		}

		parts := strings.Split(f.Path, "/")
		for j := 1; j < len(parts); j++ {
			dir := strings.Join(parts[:j], "/")

			isDir, ok := seen[dir]
			if ok && !isDir {
				return fmt.Errorf("cpio entry %q: parent %q is a file", f.Path, dir)
			}

			if !ok {
				seen[dir] = true

				entries = append(entries, entry{path: dir, mode: sIFDIR | cpioDirMode})
			}
		}

		if _, ok := seen[f.Path]; ok {
			return fmt.Errorf("cpio entry %q: duplicate path", f.Path)
		}

		seen[f.Path] = f.Mode.IsDir()

		mode := uint32(sIFREG)
		if f.Mode.IsDir() {
			mode = sIFDIR
		}

		entries = append(entries, entry{path: f.Path, mode: mode | uint32(f.Mode.Perm()), data: f.Data})
	}

	bw := bufio.NewWriter(w)

	for i, e := range entries {
		nlink := 1
		if e.mode&sIFDIR != 0 {
			nlink = 2
		}

		if err := writeCPIOEntry(bw, uint32(i+1), e.mode, uint32(nlink), e.path, e.data); err != nil {
			return err
		}
	}

	if err := writeCPIOEntry(bw, 0, 0, 1, cpioTrailer, nil); err != nil {
		return err
	}

	return bw.Flush()
}

func validCPIOPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}

	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute path %q", p)
	}

	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("path %q contains NUL", p)
	}

	if p == cpioTrailer {
		return fmt.Errorf("reserved path %q", p)
	}

	for _, c := range strings.Split(p, "/") {
		switch c {
		case "":
			return fmt.Errorf("path %q has an empty component", p)
		case ".", "..":
			return fmt.Errorf("path %q has a %q component", p, c)
		}
	}

	return nil
}

func writeCPIOEntry(w *bufio.Writer, ino, mode, nlink uint32, name string, data []byte) error {
	nameSize := len(name) + 1
	// ino mode uid gid nlink mtime filesize devmajor devminor rdevmajor rdevminor namesize check
	if _, err := fmt.Fprintf(w, "%s%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		cpioMagic, ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, nameSize, 0); err != nil {
		return err
	}

	if _, err := w.WriteString(name); err != nil {
		return err
	}

	// The 110-byte header plus the NUL-terminated name is padded to 4 bytes.
	if _, err := w.Write(make([]byte, 1+pad4(110+nameSize))); err != nil {
		return err
	}

	if _, err := w.Write(data); err != nil {
		return err
	}

	_, err := w.Write(make([]byte, pad4(len(data))))

	return err
}

func pad4(n int) int {
	return (4 - n%4) % 4
}
