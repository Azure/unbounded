// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostroot

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// trustedOwners are the users that may own a directory on the way to the
// agent's files. Systemd runs those files as root, so anyone else who owns
// such a directory, or can write to it, can replace what root runs. Tests add
// the user running them.
var trustedOwners = []uint32{0}

// maxLinks bounds the symlinks followed on the way to the root, as the kernel
// bounds them.
const maxLinks = 40

// errUntrusted is wrapped by every refusal of checkRoot.
var errUntrusted = errors.New("a user other than root could replace the agent's files")

// checkRoot refuses a host where someone other than root could replace the
// agent's files under root: a directory on the way to root, or root itself,
// that an untrusted user owns or that group or others can write to. Links are
// followed, so a link an operator made at root, or along the way to it, is
// checked where it leads.
//
// A root linked to legacy is checked up to the link and no further. Every
// release before Path trusted legacy as it is, and some distributions make it
// group-writable on purpose, so refusing it would refuse hosts those releases
// run on.
func checkRoot(root, legacy string) error {
	info, err := os.Lstat(root)

	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("inspect %s: %w", root, err)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(root)
		if err != nil {
			return fmt.Errorf("read %s: %w", root, err)
		}

		if target == legacy {
			return checkPath(filepath.Dir(root), trustedOwners)
		}
	}

	return checkPath(root, trustedOwners)
}

// checkPath checks every directory that looking up path goes through,
// following symlinks as the kernel does, and path itself when it is a
// directory, since files are created in it. It stops at the first name that
// does not exist: whatever the agent creates from there is created in a
// directory already checked.
//
// A directory passes when one of owners owns it and neither group nor others
// can write to it. A sticky directory that others can write to, such as /tmp,
// passes for a name in it that exists and that one of owners owns, because
// only that owner can then rename or remove it.
func checkPath(path string, owners []uint32) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("check %s: not an absolute path", path)
	}

	dir := "/"

	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", dir, err)
	}

	if err := checkOwner(dir, dirInfo, owners); err != nil {
		return err
	}

	pending := splitPath(path)
	links := 0

	for len(pending) > 0 {
		name := pending[0]
		pending = pending[1:]

		switch name {
		case "", ".":
			continue
		case "..":
			// Every ancestor of dir was traversed on the way down to it.
			dir = filepath.Dir(dir)

			if dirInfo, err = os.Lstat(dir); err != nil {
				return fmt.Errorf("inspect %s: %w", dir, err)
			}

			continue
		}

		next := filepath.Join(dir, name)

		info, err := os.Lstat(next)
		if errors.Is(err, os.ErrNotExist) {
			// The rest is created in dir, by the agent or by whoever else
			// can write to it first.
			return checkWritable(dir, dirInfo, nil, owners)
		}

		if err != nil {
			return fmt.Errorf("inspect %s: %w", next, err)
		}

		if err := checkWritable(dir, dirInfo, info, owners); err != nil {
			return err
		}

		if info.Mode()&os.ModeSymlink != 0 {
			links++
			if links > maxLinks {
				return fmt.Errorf("check %s: too many levels of symbolic links", path)
			}

			target, err := os.Readlink(next)
			if err != nil {
				return fmt.Errorf("read %s: %w", next, err)
			}

			if filepath.IsAbs(target) {
				dir = "/"

				if dirInfo, err = os.Lstat(dir); err != nil {
					return fmt.Errorf("inspect %s: %w", dir, err)
				}
			}

			pending = append(splitPath(target), pending...)

			continue
		}

		if !info.IsDir() {
			if len(pending) > 0 {
				return fmt.Errorf("check %s: %s is not a directory", path, next)
			}

			// A file at the end, which whoever looks it up decides about.
			return nil
		}

		if err := checkOwner(next, info, owners); err != nil {
			return err
		}

		dir, dirInfo = next, info
	}

	return checkWritable(dir, dirInfo, nil, owners)
}

func splitPath(path string) []string {
	return strings.Split(strings.Trim(path, "/"), "/")
}

// checkOwner refuses a directory none of owners owns. Its owner could change
// its mode, and with it what is in it, whatever the mode is now.
func checkOwner(dir string, info os.FileInfo, owners []uint32) error {
	if uid, ok := ownedBy(info, owners); !ok {
		return fmt.Errorf("%w: %s is owned by uid %d; make it owned by root", errUntrusted, dir, uid)
	}

	return nil
}

// checkWritable refuses a directory that group or others can write to,
// unless it is sticky and entry, the name looked up in it, exists and is owned
// by one of owners.
func checkWritable(dir string, info, entry os.FileInfo, owners []uint32) error {
	mode := info.Mode()
	if mode.Perm()&0o022 == 0 {
		return nil
	}

	if mode&os.ModeSticky != 0 && entry != nil {
		if _, ok := ownedBy(entry, owners); ok {
			return nil
		}
	}

	return fmt.Errorf("%w: %s can be written to by group or others (mode %04o); remove that access, for example with chmod go-w %s",
		errUntrusted, dir, uint32(mode.Perm()), dir)
}

func ownedBy(info os.FileInfo, owners []uint32) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}

	for _, owner := range owners {
		if stat.Uid == owner {
			return stat.Uid, true
		}
	}

	return stat.Uid, false
}
