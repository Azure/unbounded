// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"strings"
	"testing"
)

func TestRemoteDaemonCommandWrapsDaemonForCleanup(t *testing.T) {
	cmd := remoteDaemonCommand("/opt/unbounded-storage", "/etc/unbounded/config.toml", true)

	mustContain(t, cmd, "sh -c '")
	mustContain(t, cmd, "trap cleanup INT TERM HUP EXIT")
	mustContain(t, cmd, "setsid \"$@\" &")
	mustContain(t, cmd, "kill -TERM -\"$pid\"")
	mustContain(t, cmd, "'sudo' '-n' '/opt/unbounded-storage' '--config' '/etc/unbounded/config.toml'")
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got := shellQuote("/tmp/bench's config.toml")
	want := "'/tmp/bench'\\''s config.toml'"
	if got != want {
		t.Fatalf("shellQuote = %q, want %q", got, want)
	}

	joined := shellJoin([]string{"sudo", "path with spaces", "quote's"})
	if strings.Count(joined, "'") < 6 {
		t.Fatalf("shellJoin did not quote arguments: %q", joined)
	}
}
