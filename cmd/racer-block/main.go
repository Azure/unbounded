// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

// Command racer-block serves one Racer object as a read-only Linux ublk block
// device. See designs/racer-block.md.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Azure/unbounded/internal/racerblock"
	"github.com/Azure/unbounded/internal/racerblock/backend"
	"github.com/Azure/unbounded/internal/racerblock/ublk"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/racersdk"
)

const (
	// maxHeaderFileBytes matches the Racer limit for opaque header values.
	maxHeaderFileBytes = 8192
	maxQueues          = 64
	maxDepth           = 4096
	// maxConnections caps the default SDK connection limit. The backend
	// fetches whole chunks and shares them between block requests, so it
	// needs far fewer fetches than queues*depth. Racer also limits client
	// connections per worker, and extra connections only queue there.
	maxConnections = 128
)

type options struct {
	cache             string
	key               racersdk.Key
	etag              string
	authorizationFile string
	metadataFile      string
	id                int
	queues            int
	depth             int
	maxIOBytes        int
	chunkSize         int64
	cacheBytes        int64
	readaheadMax      int64
	warmTTL           time.Duration
	maxConnections    int
	logLevel          slog.Level
}

// errHelp reports that help or version output was written and the command
// should exit successfully.
var errHelp = errors.New("help requested")

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, errHelp) {
			return
		}

		fmt.Fprintln(os.Stderr, "racer-block:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, err := parseOptions(args, stdout)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: o.logLevel}))
	logger.Info("starting racer-block", "version", version.String(), "cache", o.cache, "key", o.key.String())

	req := racersdk.Request{Key: o.key}

	if req.Authorization, err = readHeaderFile(o.authorizationFile, "--authorization-file"); err != nil {
		return err
	}

	if req.Metadata, err = readHeaderFile(o.metadataFile, "--metadata-file"); err != nil {
		return err
	}

	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: o.cache, MaxConnections: o.maxConnections})
	if err != nil {
		return fmt.Errorf("create Racer client: %w", err)
	}

	defer func() {
		if err := client.Close(); err != nil {
			logger.Warn("close Racer client", "err", err)
		}
	}()

	return racerblock.Run(ctx, racerblock.Config{
		Source: backend.FromClient(client, req),
		Backend: backend.Config{
			ETag:         o.etag,
			ChunkSize:    o.chunkSize,
			CacheBytes:   o.cacheBytes,
			ReadaheadMax: o.readaheadMax,
			WarmTTL:      o.warmTTL,
		},
		Device: ublk.Config{
			ID:         o.id,
			Queues:     o.queues,
			Depth:      o.depth,
			MaxIOBytes: o.maxIOBytes,
		},
		ReplaceExisting: o.id >= 0,
		Ready: func(path string) {
			if _, err := fmt.Fprintln(stdout, path); err != nil {
				logger.Warn("print device path", "err", err)
			}
		},
		Logger: logger,
	})
}

func parseOptions(args []string, output io.Writer) (options, error) {
	o := options{}

	fs := flag.NewFlagSet("racer-block", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var helpErr error

	fs.Usage = func() {
		var defaults bytes.Buffer

		fs.SetOutput(&defaults)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)

		helpErr = writeOutput(output, "Usage: racer-block --cache NAME --key HEX [flags]\n"+
			"Serves one Racer object as a read-only ublk block device and prints its path.\n"+defaults.String())
	}

	fs.StringVar(&o.cache, "cache", "", "Racer cache name (ClusterCache); required")
	key := fs.String("key", "", "Object key as 64 lowercase hex characters; required")
	fs.StringVar(&o.etag, "etag", "", "Strong ETag to pin, with quotes; empty pins the current version")
	fs.StringVar(&o.authorizationFile, "authorization-file", "", "File with the opaque Authorization value sent to the origin on a miss")
	fs.StringVar(&o.metadataFile, "metadata-file", "", "File with the opaque Racer-Metadata value sent to the origin on a miss")
	fs.IntVar(&o.id, "id", -1, "ublk device ID; -1 lets the kernel choose. A fixed ID replaces a device left behind with that ID")
	fs.IntVar(&o.queues, "queues", 0, "ublk hardware queues; 0 uses min(CPUs, 4)")
	fs.IntVar(&o.depth, "depth", 0, "ublk queue depth; 0 uses 128")
	fs.IntVar(&o.maxIOBytes, "max-io-bytes", 0, "Largest single block request in bytes; 0 uses 1 MiB")
	fs.Int64Var(&o.chunkSize, "chunk-size", 0, "Cache chunk size in bytes, a power of two from 64 KiB to 16 MiB; 0 uses 4 MiB")
	fs.Int64Var(&o.cacheBytes, "cache-bytes", 0, "Chunk cache size in bytes; 0 uses 1 GiB")
	fs.Int64Var(&o.readaheadMax, "readahead-max", 0, "Largest sequential readahead window in bytes; 0 uses 64 MiB, negative disables readahead")
	fs.DurationVar(&o.warmTTL, "warm-ttl", 0, "Ask Racer to retain the pinned version for this long, refreshed at half the TTL; 0 disables")
	fs.IntVar(&o.maxConnections, "max-connections", 0, "Concurrent Racer connections; 0 uses queues*depth, capped at 128")

	logLevel := fs.String("log-level", "info", "Log level: debug, info, warn, or error")
	showVersion := fs.Bool("version", false, "Print version")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			if helpErr != nil {
				return o, helpErr
			}

			return o, errHelp
		}

		return o, fmt.Errorf("invalid flags: %w (use --help)", err)
	}

	if fs.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}

	if *showVersion {
		if err := writeOutput(output, version.String()+"\n"); err != nil {
			return o, err
		}

		return o, errHelp
	}

	if err := o.logLevel.UnmarshalText([]byte(*logLevel)); err != nil {
		return o, errors.New("invalid --log-level")
	}

	if err := o.validate(*key); err != nil {
		return o, err
	}

	return o, nil
}

func (o *options) validate(key string) error {
	if o.cache == "" {
		return errors.New("--cache is required")
	}

	validationClient, err := racersdk.NewClient(racersdk.ClientConfig{Cache: o.cache})
	if err != nil {
		return errors.New("invalid --cache")
	}

	if err := validationClient.Close(); err != nil {
		return fmt.Errorf("invalid --cache: %w", err)
	}

	if key == "" {
		return errors.New("--key is required")
	}

	if o.key, err = racersdk.ParseKey(key); err != nil {
		return errors.New("invalid --key: want 64 lowercase hex characters")
	}

	if o.id < -1 {
		return errors.New("--id must be -1 or a device ID")
	}

	if o.queues < 0 || o.queues > maxQueues {
		return fmt.Errorf("--queues must be between 0 and %d", maxQueues)
	}

	if o.depth < 0 || o.depth > maxDepth {
		return fmt.Errorf("--depth must be between 0 and %d", maxDepth)
	}

	if o.maxIOBytes < 0 {
		return errors.New("--max-io-bytes must not be negative")
	}

	if o.chunkSize < 0 {
		return errors.New("--chunk-size must not be negative")
	}

	if o.cacheBytes < 0 {
		return errors.New("--cache-bytes must not be negative")
	}

	if o.warmTTL < 0 {
		return errors.New("--warm-ttl must not be negative")
	}

	if o.maxConnections < 0 {
		return errors.New("--max-connections must not be negative")
	}

	if o.maxConnections == 0 {
		o.maxConnections = defaultConnections(o.queues, o.depth)
	}

	return nil
}

// defaultConnections sizes the SDK connection limit so every in-flight block
// request can have its own fetch, capped to keep Racer socket use bounded.
func defaultConnections(queues, depth int) int {
	if queues == 0 {
		queues = ublk.DefaultQueues()
	}

	if depth == 0 {
		depth = ublk.DefaultDepth
	}

	return min(queues*depth, maxConnections)
}

func readHeaderFile(path, flagName string) (value string, err error) {
	if path == "" {
		return "", nil
	}

	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("%s: %w", flagName, err)
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%s: %w", flagName, cerr)
		}
	}()

	data, err := io.ReadAll(io.LimitReader(f, maxHeaderFileBytes+2))
	if err != nil {
		return "", fmt.Errorf("%s: %w", flagName, err)
	}

	data = bytes.TrimSuffix(data, []byte("\n"))
	data = bytes.TrimSuffix(data, []byte("\r"))

	if len(data) > maxHeaderFileBytes {
		return "", fmt.Errorf("%s: value is longer than %d bytes", flagName, maxHeaderFileBytes)
	}

	return string(data), nil
}

func writeOutput(w io.Writer, text string) error {
	if _, err := io.WriteString(w, text); err != nil {
		return err
	}

	return errHelp
}
