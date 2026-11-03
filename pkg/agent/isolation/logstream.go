// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package isolation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// JournalTailArgs returns the journalctl flags for the follow and line-count
// options. Each backend supplies the flags that select its units or machine,
// because they differ by mode.
func JournalTailArgs(opts LogOptions) []string {
	var args []string

	if opts.Follow {
		args = append(args, "--follow")
	}

	if opts.Lines > 0 {
		args = append(args, "--lines="+strconv.Itoa(opts.Lines))
	}

	return args
}

// maxStderrReport bounds how much of a failed command's stderr is kept for the
// error.
const maxStderrReport = 2048

// StartLogStream starts a command and returns its standard output. Closing the
// reader waits for the command, so a stream that was cut short does not leave a
// zombie. When the command ran to the end of its output and exited non-zero,
// for example journalctl against a machine that does not exist, Close returns an
// error carrying the end of its stderr. Stopping the stream early, by closing it
// or canceling ctx, is not an error.
func StartLogStream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, name, args...)

	stderr := &tailBuffer{max: maxStderrReport}
	cmd.Stderr = stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	return &cmdReader{ReadCloser: stdout, cmd: cmd, ctx: ctx, name: name, stderr: stderr}, nil
}

type cmdReader struct {
	io.ReadCloser
	cmd    *exec.Cmd
	ctx    context.Context //nolint:containedctx // the stream's lifetime is the command's, and Close must know whether it was canceled
	name   string
	stderr *tailBuffer
	// ended is set when Read reached the end of the command's output.
	ended bool
}

func (c *cmdReader) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	if errors.Is(err, io.EOF) {
		c.ended = true
	}

	return n, err
}

func (c *cmdReader) Close() error {
	err := c.ReadCloser.Close()
	waitErr := c.cmd.Wait()

	if err != nil {
		return err
	}

	if waitErr == nil || !c.ended || c.ctx.Err() != nil {
		return nil
	}

	if reason := strings.TrimSpace(c.stderr.String()); reason != "" {
		return fmt.Errorf("%s: %w: %s", c.name, waitErr, reason)
	}

	return fmt.Errorf("%s: %w", c.name, waitErr)
}

// tailBuffer keeps the last max bytes written to it. A command that writes more
// than that is never blocked: Write always reports the full length.
type tailBuffer struct {
	mu   sync.Mutex
	max  int
	data []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.data = append(b.data, p...)
	if len(b.data) > b.max {
		b.data = b.data[len(b.data)-b.max:]
	}

	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	return string(b.data)
}
