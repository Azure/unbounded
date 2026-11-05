// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package hostnode

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"github.com/Azure/unbounded/internal/executil"
	"github.com/Azure/unbounded/pkg/agent/isolation/none"
)

// UnmountBelow lazily unmounts every mount at or below the given prefixes,
// deepest first. Pod volumes (tmpfs secrets, projected volumes) are mounted
// under /var/lib/kubelet and can outlive the node stack; they would make
// removing the directory fail or, worse, delete through a bind mount.
//
// mountInfo is the mount table (empty means /proc/self/mountinfo) and run
// executes host commands (nil uses the real host).
func UnmountBelow(ctx context.Context, log *slog.Logger, run none.Runner, mountInfo string, prefixes ...string) error {
	failed, err := unmountBelow(ctx, log, run, mountInfo, prefixes...)
	if err != nil {
		return err
	}

	var errs []error

	for _, p := range prefixes {
		if failed[p] != nil {
			errs = append(errs, failed[p])
		}
	}

	return errors.Join(errs...)
}

// UnmountBelowByPrefix is UnmountBelow with the failures attributed to the
// prefix whose mounts could not be unmounted, in one deepest-first pass over all
// prefixes. Callers use it to keep the directories that still have mounts below
// them and to remove the rest. The error is for an unreadable mount table, in
// which case no prefix can be trusted.
func UnmountBelowByPrefix(ctx context.Context, log *slog.Logger, run none.Runner, mountInfo string, prefixes ...string) (map[string]error, error) {
	return unmountBelow(ctx, log, run, mountInfo, prefixes...)
}

func unmountBelow(ctx context.Context, log *slog.Logger, run none.Runner, mountInfo string, prefixes ...string) (map[string]error, error) {
	log = Logger(log)

	if mountInfo == "" {
		mountInfo = "/proc/self/mountinfo"
	}

	f, err := os.Open(mountInfo)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	defer f.Close() //nolint:errcheck // read only

	type mount struct{ point, prefix string }

	var mounts []mount

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 5 {
			continue
		}

		mp := unescapeMount(fields[4])
		for _, p := range prefixes {
			if mp == p || strings.HasPrefix(mp, p+"/") {
				mounts = append(mounts, mount{point: mp, prefix: p})
			}
		}
	}

	if err := sc.Err(); err != nil {
		return nil, err
	}

	sort.Slice(mounts, func(i, j int) bool { return len(mounts[i].point) > len(mounts[j].point) })

	if run == nil {
		run = func(ctx context.Context, log *slog.Logger, name string, args ...string) (string, error) {
			return executil.OutputCmd(ctx, log, name, args...)
		}
	}

	failed := map[string]error{}

	for _, m := range mounts {
		if _, err := run(ctx, log, "umount", "-l", m.point); err != nil {
			failed[m.prefix] = errors.Join(failed[m.prefix], fmt.Errorf("unmount %s: %w", m.point, err))
		}
	}

	return failed, nil
}

// unescapeMount decodes the octal escapes mountinfo uses for spaces and tabs.
func unescapeMount(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}
