// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go/logging"
	"golang.org/x/sys/unix"

	"github.com/Azure/unbounded/internal/racerobject"
	"github.com/Azure/unbounded/internal/version"
	"github.com/Azure/unbounded/pkg/racersdk"
)

type bucketsFlag []string

func (b *bucketsFlag) String() string { return strings.Join(*b, ",") }
func (b *bucketsFlag) Set(value string) error {
	*b = append(*b, value)
	return nil
}

type options struct {
	mode           string
	volume         string
	namespace      string
	buckets        bucketsFlag
	region         string
	endpoint       string
	pathStyle      bool
	metadataTTL    time.Duration
	requestTimeout time.Duration
	listen         string
	debugListen    string
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "racer-object:", err)
		os.Exit(1)
	}
}

func parseOptions(args []string, output io.Writer) (options, error) {
	var o options
	if len(args) == 0 {
		return o, errors.New("expected origin or sidecar (use --help)")
	}

	switch args[0] {
	case "--help", "-h":
		return o, writeHelp(output, "Usage: racer-object <origin|sidecar> [flags]\nUse racer-object <command> --help for flags.\n")
	case "--version":
		return o, writeHelp(output, version.String()+"\n")
	case "origin", "sidecar":
		o.mode = args[0]
	default:
		return o, errors.New("unknown command (use --help)")
	}

	fs := flag.NewFlagSet(o.mode, flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	var helpErr error

	fs.Usage = func() {
		var defaults bytes.Buffer

		fs.SetOutput(&defaults)
		fs.PrintDefaults()
		fs.SetOutput(io.Discard)

		helpErr = writeHelp(output, "Usage: racer-object "+o.mode+" [flags]\n"+defaults.String())
	}
	volume := fs.String("volume", "racer-object", "Racer volume name")
	fs.StringVar(&o.namespace, "namespace", "", "Required stable upstream identity; use the same value in origin and sidecar")
	fs.Var(&o.buckets, "bucket", "Allowed bucket (repeatable; omitted allows all buckets)")
	fs.StringVar(&o.debugListen, "debug-listen", "", "Optional pprof address (e.g. 127.0.0.1:6060); disabled when empty. Trusted diagnostic access only: nonloopback addresses such as :6060 expose unauthenticated profiles")

	showVersion := fs.Bool("version", false, "Print version")
	if o.mode == "origin" {
		fs.StringVar(&o.region, "region", "", "AWS region (defaults to AWS configuration and environment)")
		fs.StringVar(&o.endpoint, "endpoint", "", "Optional HTTP(S) upstream origin, without a path or credentials")
		fs.BoolVar(&o.pathStyle, "path-style", true, "Use path-style upstream addressing")
		fs.DurationVar(&o.metadataTTL, "metadata-ttl", 30*time.Second, "Metadata lifetime; zero expires immediately")
		fs.DurationVar(&o.requestTimeout, "request-timeout", time.Minute, "Positive origin request timeout")
	} else {
		fs.StringVar(&o.listen, "listen", "127.0.0.1:8080", "Loopback IP literal and port")
		fs.DurationVar(&o.requestTimeout, "request-timeout", 5*time.Minute, "Positive sidecar request timeout")
	}

	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, helpErr
		}

		return o, errors.New("invalid flags (use --help)")
	}

	if fs.NArg() != 0 {
		return o, errors.New("unexpected positional arguments")
	}

	if *showVersion {
		return o, writeHelp(output, version.String()+"\n")
	}

	o.volume = *volume

	validationClient, err := racersdk.NewClient(racersdk.ClientConfig{Volume: o.volume})
	if err != nil {
		return o, errors.New("invalid --volume")
	}

	closeResource(validationClient)

	if _, err := racerobject.NewRequest(o.namespace, "validation", "validation", ""); err != nil {
		return o, errors.New("--namespace must be a valid stable upstream identity")
	}

	for _, bucket := range o.buckets {
		if _, err := racerobject.NewRequest(o.namespace, bucket, "validation", ""); err != nil {
			return o, errors.New("invalid --bucket")
		}
	}

	if o.requestTimeout <= 0 {
		return o, errors.New("--request-timeout must be positive")
	}

	if o.mode == "origin" {
		if o.metadataTTL < 0 {
			return o, errors.New("--metadata-ttl must not be negative")
		}

		if err := validateEndpoint(o.endpoint); err != nil {
			return o, err
		}
	} else if err := validateListen(o.listen); err != nil {
		return o, err
	}

	return o, nil
}

func writeHelp(output io.Writer, text string) error {
	if _, err := io.WriteString(output, text); err != nil {
		return errors.New("could not write CLI output")
	}

	return flag.ErrHelp
}

func closeResource(c io.Closer) {
	if err := c.Close(); err != nil {
		return
	}
}

func validateEndpoint(value string) error {
	if value == "" {
		return nil
	}

	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || strings.Contains(value, "#") ||
		(u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return errors.New("--endpoint must be an HTTP(S) origin without credentials, query, fragment, or path prefix")
	}

	if strings.HasSuffix(u.Host, ":") {
		return errors.New("invalid --endpoint port")
	}

	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return errors.New("invalid --endpoint port")
		}
	}

	return nil
}

func validateListen(value string) error {
	host, port, err := net.SplitHostPort(value)

	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() {
		return errors.New("--listen must use a loopback IP literal and port")
	}

	if port == "" || strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) != -1 {
		return errors.New("--listen port must be between 0 and 65535")
	}

	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("--listen port must be between 0 and 65535")
	}

	return nil
}

func run(ctx context.Context, args []string, output io.Writer) error {
	o, err := parseOptions(args, output)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}

	if err != nil {
		return err
	}

	if ctx.Err() != nil {
		return nil
	}

	return runWithDebug(ctx, o.debugListen, func(ctx context.Context) error {
		if o.mode == "origin" {
			return runOrigin(ctx, o)
		}

		return runSidecar(ctx, o)
	})
}

func originAWSConfig(ctx context.Context, o options) (aws.Config, error) {
	load := []func(*config.LoadOptions) error{
		config.WithHTTPClient(&http.Client{Timeout: o.requestTimeout}),
		config.WithLogger(logging.NewStandardLogger(io.Discard)),
	}
	if o.region != "" {
		load = append(load, config.WithRegion(o.region))
	}

	cfg, err := config.LoadDefaultConfig(ctx, load...)
	if err != nil {
		return aws.Config{}, errors.New("could not load AWS configuration")
	}

	if strings.TrimSpace(cfg.Region) == "" {
		return aws.Config{}, errors.New("AWS region is required; set --region or AWS_REGION")
	}

	return cfg, nil
}

func runOrigin(ctx context.Context, o options) error {
	cfg, err := originAWSConfig(ctx, o)
	if err != nil {
		return err
	}

	client := s3.NewFromConfig(cfg, func(s *s3.Options) {
		s.UsePathStyle = o.pathStyle
		if o.endpoint != "" {
			s.BaseEndpoint = aws.String(o.endpoint)
		}
	})

	origin, err := racerobject.NewOrigin(client, racerobject.OriginConfig{
		Namespace: o.namespace, Buckets: o.buckets, MetadataTTL: o.metadataTTL,
	})
	if err != nil {
		return errors.New("could not configure origin")
	}

	root, err := os.Open("/")
	if err != nil {
		return errors.New("could not open origin directory root")
	}
	defer closeResource(root)

	if err := prepareOriginDirectory(root, []string{"run", "racer", o.volume, "origin"}); err != nil {
		return errors.New("could not prepare safe origin directory")
	}

	err = racersdk.ServeOrigin(ctx, racersdk.OriginConfig{
		Volume: o.volume, RecoverStaleSocket: true,
	}, origin)
	if ctx.Err() != nil {
		return nil
	}

	if err != nil {
		return errors.New("origin server failed")
	}

	return nil
}

// Walk from a trusted descriptor without following symlinks or changing existing permissions.
func prepareOriginDirectory(parent *os.File, parts []string) error {
	info, err := parent.Stat()
	if err != nil {
		return err
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || info.Mode().Perm()&0o022 != 0 ||
		(stat.Uid != 0 && stat.Uid != uint32(os.Geteuid())) {
		return os.ErrPermission
	}

	if len(parts) == 0 {
		if stat.Uid != uint32(os.Geteuid()) {
			return os.ErrPermission
		}

		return nil
	}

	part := parts[0]
	if part == "" || part == "." || part == ".." || strings.Contains(part, "/") {
		return os.ErrInvalid
	}

	fd, err := unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		if err := unix.Mkdirat(int(parent.Fd()), part, 0o755); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}

		fd, err = unix.Openat(int(parent.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	}

	if err != nil {
		return err
	}

	child := os.NewFile(uintptr(fd), part)
	defer closeResource(child)

	return prepareOriginDirectory(child, parts[1:])
}

func runSidecar(ctx context.Context, o options) error {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Volume: o.volume})
	if err != nil {
		return errors.New("could not configure Racer client")
	}
	defer closeResource(client)

	handler, err := racerobject.NewSidecar(client, racerobject.SidecarConfig{Namespace: o.namespace, Buckets: o.buckets})
	if err != nil {
		return errors.New("could not configure sidecar")
	}

	listener, err := listenSidecar(ctx, o.listen)
	if err != nil {
		return err
	}

	return serveSidecar(ctx, listener, handler, o.requestTimeout)
}

func listenSidecar(ctx context.Context, address string) (net.Listener, error) {
	if err := validateListen(address); err != nil {
		return nil, err
	}

	var lc net.ListenConfig

	l, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		return nil, errors.New("could not bind sidecar listener")
	}

	return l, nil
}

func serveSidecar(ctx context.Context, listener net.Listener, handler http.Handler, requestTimeout time.Duration) error {
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()

	server := &http.Server{
		ReadHeaderTimeout: min(5*time.Second, requestTimeout),
		ReadTimeout:       requestTimeout, WriteTimeout: requestTimeout, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 32 << 10, ErrorLog: log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return lifetime },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestCtx, done := context.WithTimeout(r.Context(), requestTimeout)
			defer done()

			handler.ServeHTTP(w, r.WithContext(requestCtx))
		}),
	}
	defer closeResource(server)
	defer closeResource(listener)

	finished := make(chan error, 1)

	go func() { finished <- server.Serve(limitSidecarListener(listener, 128)) }()

	select {
	case err := <-finished:
		if ctx.Err() != nil {
			return nil
		}

		if err != nil {
			return errors.New("sidecar server failed")
		}

		return nil
	case <-ctx.Done():
		cancel()

		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()

		if err := server.Shutdown(shutdownCtx); err != nil {
			closeResource(server)
		}

		<-finished

		return nil
	}
}
