// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package qemusvr

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// setEFIBoundary makes the HTTP boundary disk visible at the next power-on.
// When enabled, it stages a FAT disk built from the cached HTTP entrypoint and
// artifacts fetched over the boundary bridge. When disabled, it restores the
// blank EFI source. The caller must hold s.mu.
func (s *State) setEFIBoundary(enabled bool) error {
	if s.cfg.EFISource == "" || s.cfg.EFIActive == "" || s.cfg.Bridge == "" || s.cfg.CacheDir == "" {
		return errors.New("UefiHttp requested without HTTP boundary arguments")
	}

	if !enabled {
		return copyFile(s.cfg.EFISource, s.cfg.EFIActive)
	}

	bootURL := asString(s.boot["HttpBootUri"])
	if bootURL == "" {
		return errors.New("UefiHttp override has no HttpBootUri")
	}

	baseURL := bootURL[:strings.LastIndex(bootURL, "/")]
	entrypoint := bootURL[strings.LastIndex(bootURL, "/")+1:]

	return s.withClientAddress(func(clientIP string) error {
		return s.stageBoundary(clientIP, baseURL, entrypoint)
	})
}

// stageBoundary downloads the boot artifacts and builds the boundary FAT image.
func (s *State) stageBoundary(clientIP, baseURL, entrypoint string) error {
	artifactDir, err := os.MkdirTemp("", "boundary-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(artifactDir) //nolint:errcheck // Best-effort cleanup of staging dir.

	candidates, err := filepath.Glob(filepath.Join(s.cfg.CacheDir, "oci", "*", "amd64", "disk", entrypoint))
	if err != nil {
		return err
	}

	if len(candidates) != 1 {
		return fmt.Errorf("expected one cached HTTP entrypoint %s, found %d", entrypoint, len(candidates))
	}

	if err := copyFile(candidates[0], filepath.Join(artifactDir, "http-entrypoint.efi")); err != nil {
		return err
	}

	downloads := []struct{ path, url string }{
		{"grubx64.efi", baseURL + "/grubx64.efi"},
		{"vmlinuz", baseURL + "/vmlinuz"},
		{"initrd", baseURL + "/initrd"},
		{"init.cpio", baseURL + "/init.cpio"},
		{"grub/grub.cfg", baseURL + "/grub/grub.cfg"},
	}
	for _, d := range downloads {
		target := filepath.Join(artifactDir, filepath.FromSlash(d.path))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}

		if _, code, err := s.runner.Run("curl", "--fail", "--silent", "--show-error",
			"--interface", clientIP, "--output", target, d.url); err != nil {
			return err
		} else if code != 0 {
			return fmt.Errorf("curl %s failed with code %d", d.url, code)
		}
	}

	boundary := filepath.Join(artifactDir, "boundary.img")
	if err := s.buildBoundaryImage(artifactDir, boundary); err != nil {
		return err
	}

	return copyFile(boundary, s.cfg.EFIActive)
}

// buildBoundaryImage creates a FAT image sized to hold the staged artifacts and
// copies them into the expected layout.
func (s *State) buildBoundaryImage(artifactDir, boundary string) error {
	artifactSize, err := dirSize(artifactDir)
	if err != nil {
		return err
	}

	const (
		minSize   = 64 * 1024 * 1024
		slackBase = 32 * 1024 * 1024
	)

	slack := int64(slackBase)
	if artifactSize/4 > slack {
		slack = artifactSize / 4
	}

	boundarySize := int64(minSize)
	if artifactSize+slack > boundarySize {
		boundarySize = artifactSize + slack
	}

	if err := os.Truncate(boundary, boundarySize); err != nil {
		f, cerr := os.Create(boundary)
		if cerr != nil {
			return cerr
		}

		_ = f.Close() //nolint:errcheck // Best-effort close before truncating the created file.

		if err := os.Truncate(boundary, boundarySize); err != nil {
			return err
		}
	}

	if err := s.checkRun("mkfs.vfat", "-n", "HTTPBOOT", boundary); err != nil {
		return err
	}

	if err := s.checkRun("mmd", "-i", boundary, "::/EFI", "::/EFI/BOOT", "::/grub"); err != nil {
		return err
	}

	copies := []struct{ source, target string }{
		{"http-entrypoint.efi", "::/EFI/BOOT/BOOTX64.EFI"},
		{"grubx64.efi", "::/EFI/BOOT/grubx64.efi"},
		{"vmlinuz", "::/vmlinuz"},
		{"initrd", "::/initrd"},
		{"init.cpio", "::/init.cpio"},
		{"grub/grub.cfg", "::/grub/grub.cfg"},
		{"grub/grub.cfg", "::/EFI/BOOT/grub.cfg"},
	}
	for _, c := range copies {
		source := filepath.Join(artifactDir, filepath.FromSlash(c.source))
		if err := s.checkRun("mcopy", "-o", "-i", boundary, source, c.target); err != nil {
			return err
		}
	}

	return nil
}

// checkRun runs a command and returns an error on non-zero exit.
func (s *State) checkRun(name string, args ...string) error {
	_, code, err := s.runner.Run(name, args...)
	if err != nil {
		return err
	}

	if code != 0 {
		return fmt.Errorf("%s %s exited with code %d", name, strings.Join(args, " "), code)
	}

	return nil
}

func dirSize(root string) (int64, error) {
	var total int64

	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.Mode().IsRegular() {
			total += info.Size()
		}

		return nil
	})

	return total, err
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck // Best-effort close of source file.

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() //nolint:errcheck // Best-effort close on the error path.

		return err
	}

	return out.Close()
}
