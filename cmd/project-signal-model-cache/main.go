// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const manifestName = "model-manifest.tsv"

var seedUIDPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

type manifestEntry struct {
	Path   string
	Digest string
	Size   int64
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command is required: verify or seed")
	}

	switch args[0] {
	case "verify":
		flags := flag.NewFlagSet("verify", flag.ContinueOnError)
		directory := flags.String("dir", "", "model cache directory")

		manifestDigest := flags.String("manifest-digest", "", "expected manifest sha256 digest")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}

		if err := verifyDirectory(*directory, *manifestDigest); err != nil {
			return err
		}

		fmt.Printf("verified model cache manifest %s\n", *manifestDigest)

		return nil
	case "seed":
		flags := flag.NewFlagSet("seed", flag.ContinueOnError)
		sourceDirectory := flags.String("source-dir", "", "verified image model directory")
		targetDirectory := flags.String("target-dir", "", "final PVC model directory")
		manifestDigest := flags.String("manifest-digest", "", "expected manifest sha256 digest")

		seedUID := flags.String("seed-uid", "", "DNS-compatible unique seed identity")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}

		if err := seedDirectory(*sourceDirectory, *targetDirectory, *manifestDigest, *seedUID); err != nil {
			return err
		}

		fmt.Printf("verified seeded model cache manifest %s\n", *manifestDigest)

		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func verifyDirectory(directory, manifestDigest string) error {
	if directory == "" {
		return errors.New("model cache directory is required")
	}

	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("stat model cache directory: %w", err)
	}

	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("model cache path is not a directory")
	}

	entries, err := readManifest(directory, manifestDigest)
	if err != nil {
		return err
	}

	expected := map[string]struct{}{manifestName: {}}

	for _, entry := range entries {
		expected[entry.Path] = struct{}{}

		path := filepath.Join(directory, filepath.FromSlash(entry.Path))

		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("stat model cache file %q: %w", entry.Path, err)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("model cache file %q is not regular", entry.Path)
		}

		if info.Size() != entry.Size {
			return fmt.Errorf(
				"model cache file %q size = %d, want %d",
				entry.Path,
				info.Size(),
				entry.Size,
			)
		}

		digest, err := fileDigest(path)
		if err != nil {
			return err
		}

		if digest != entry.Digest {
			return fmt.Errorf(
				"model cache file %q digest = %s, want %s",
				entry.Path,
				digest,
				entry.Digest,
			)
		}
	}

	return filepath.WalkDir(directory, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if path == directory {
			return nil
		}

		relative, err := filepath.Rel(directory, path)
		if err != nil {
			return err
		}

		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			return fmt.Errorf("model cache contains unexpected directory %q", relative)
		}

		if _, exists := expected[relative]; !exists {
			return fmt.Errorf("model cache contains unexpected file %q", relative)
		}

		return nil
	})
}

func readManifest(directory, expectedDigest string) ([]manifestEntry, error) {
	if !strings.HasPrefix(expectedDigest, "sha256:") || len(expectedDigest) != len("sha256:")+64 {
		return nil, errors.New("manifest digest must be an algorithm-qualified sha256 digest")
	}

	manifestPath := filepath.Join(directory, manifestName)

	content, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read model cache manifest: %w", err)
	}

	actualDigest := digestBytes(content)
	if actualDigest != expectedDigest {
		return nil, fmt.Errorf("model cache manifest digest = %s, want %s", actualDigest, expectedDigest)
	}

	if len(content) == 0 || content[len(content)-1] != '\n' {
		return nil, errors.New("model cache manifest must end with a newline")
	}

	var entries []manifestEntry

	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			return nil, errors.New("model cache manifest contains an empty line")
		}

		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			return nil, fmt.Errorf("model cache manifest line %q does not have three tab-separated fields", line)
		}

		path := fields[0]
		if path == "" || path == manifestName || strings.Contains(path, "/") || filepath.IsAbs(path) ||
			filepath.ToSlash(filepath.Clean(path)) != path ||
			strings.HasPrefix(path, "../") {
			return nil, fmt.Errorf("model cache manifest path %q is invalid", path)
		}

		digest := fields[1]
		if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
			return nil, fmt.Errorf("model cache manifest digest %q is invalid", digest)
		}

		if _, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:")); err != nil {
			return nil, fmt.Errorf("decode model cache manifest digest %q: %w", digest, err)
		}

		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("model cache manifest size %q is invalid", fields[2])
		}

		entries = append(entries, manifestEntry{Path: path, Digest: digest, Size: size})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan model cache manifest: %w", err)
	}

	if len(entries) == 0 {
		return nil, errors.New("model cache manifest is empty")
	}

	paths := make([]string, len(entries))
	for index := range entries {
		paths[index] = entries[index].Path
	}

	if !slices.IsSorted(paths) {
		return nil, errors.New("model cache manifest paths are not lexicographically sorted")
	}

	for index := 1; index < len(paths); index++ {
		if paths[index] == paths[index-1] {
			return nil, fmt.Errorf("model cache manifest path %q is duplicated", paths[index])
		}
	}

	return entries, nil
}

func seedDirectory(sourceDirectory, targetDirectory, manifestDigest, seedUID string) (resultErr error) {
	if sourceDirectory == "" || targetDirectory == "" {
		return errors.New("source and target model cache directories are required")
	}

	if !seedUIDPattern.MatchString(seedUID) {
		return errors.New("seed UID must be a DNS-compatible lowercase identity")
	}

	if err := verifyDirectory(sourceDirectory, manifestDigest); err != nil {
		return fmt.Errorf("verify source model cache: %w", err)
	}

	if _, err := os.Stat(targetDirectory); err == nil {
		if err := verifyDirectory(targetDirectory, manifestDigest); err != nil {
			return fmt.Errorf("existing target model cache is invalid: %w", err)
		}

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat target model cache: %w", err)
	}

	parent := filepath.Dir(targetDirectory)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create model cache parent: %w", err)
	}

	temporaryDirectory := targetDirectory + ".seed-" + seedUID
	if err := os.Mkdir(temporaryDirectory, 0o755); err != nil {
		return fmt.Errorf("create temporary model cache: %w", err)
	}

	complete := false
	defer func() {
		if !complete {
			if err := os.RemoveAll(temporaryDirectory); err != nil {
				resultErr = errors.Join(
					resultErr,
					fmt.Errorf("remove temporary model cache: %w", err),
				)
			}
		}
	}()

	if err := copyDirectory(sourceDirectory, temporaryDirectory); err != nil {
		return err
	}

	if err := verifyDirectory(temporaryDirectory, manifestDigest); err != nil {
		return fmt.Errorf("verify copied model cache: %w", err)
	}

	if err := publishDirectory(temporaryDirectory, targetDirectory, manifestDigest); err != nil {
		return err
	}

	complete = true

	return nil
}

func publishDirectory(temporaryDirectory, targetDirectory, manifestDigest string) error {
	if err := os.Rename(temporaryDirectory, targetDirectory); err != nil {
		if verifyErr := verifyDirectory(targetDirectory, manifestDigest); verifyErr != nil {
			return errors.Join(
				fmt.Errorf("publish model cache: %w", err),
				fmt.Errorf("verify concurrently published model cache: %w", verifyErr),
			)
		}

		if removeErr := os.RemoveAll(temporaryDirectory); removeErr != nil {
			return fmt.Errorf("remove losing temporary model cache: %w", removeErr)
		}
	}

	return nil
}

func copyDirectory(sourceDirectory, targetDirectory string) error {
	return filepath.WalkDir(sourceDirectory, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		relative, err := filepath.Rel(sourceDirectory, sourcePath)
		if err != nil {
			return err
		}

		if relative == "." {
			return nil
		}

		targetPath := filepath.Join(targetDirectory, relative)
		if entry.IsDir() {
			if err := os.Mkdir(targetPath, 0o755); err != nil {
				return fmt.Errorf("create model cache directory %q: %w", relative, err)
			}

			return nil
		}

		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("model cache source %q is a symbolic link", relative)
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("stat model cache source %q: %w", relative, err)
		}

		if !info.Mode().IsRegular() {
			return fmt.Errorf("model cache source %q is not regular", relative)
		}

		return copyFile(sourcePath, targetPath, relative)
	})
}

func copyFile(sourcePath, targetPath, relative string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("open model cache source %q: %w", relative, err)
	}

	target, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
	if err != nil {
		return errors.Join(
			fmt.Errorf("create model cache target %q: %w", relative, err),
			closeFile(source, "model cache source "+relative),
		)
	}

	if _, err := io.Copy(target, source); err != nil {
		return errors.Join(
			fmt.Errorf("copy model cache file %q: %w", relative, err),
			closeFile(source, "model cache source "+relative),
			closeFile(target, "model cache target "+relative),
		)
	}

	if err := closeFile(source, "model cache source "+relative); err != nil {
		return errors.Join(err, closeFile(target, "model cache target "+relative))
	}

	if err := target.Sync(); err != nil {
		return errors.Join(
			fmt.Errorf("sync model cache file %q: %w", relative, err),
			closeFile(target, "model cache target "+relative),
		)
	}

	if err := closeFile(target, "model cache target "+relative); err != nil {
		return err
	}

	return nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", errors.Join(
			fmt.Errorf("digest %s: %w", path, err),
			closeFile(file, path),
		)
	}

	if err := closeFile(file, path); err != nil {
		return "", err
	}

	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func closeFile(file *os.File, description string) error {
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", description, err)
	}

	return nil
}

func digestBytes(content []byte) string {
	sum := sha256.Sum256(content)

	return "sha256:" + hex.EncodeToString(sum[:])
}
