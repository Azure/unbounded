// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// newSymlinkRootCommand builds symlink management commands.
func newSymlinkRootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:     "symlink",
		Aliases: []string{"symlinks"},
		Short:   "Manage kubectl plugin symlinks (completion + create shortcuts)",
	}

	var force bool

	createCmd := &cobra.Command{
		Use:   "create",
		Short: "Create plugin symlinks in the binary directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}

			exeDir := filepath.Dir(exe)
			exeBase := filepath.Base(exe)

			for _, linkName := range symlinkCreateNames {
				linkPath := filepath.Join(exeDir, linkName)

				info, statErr := os.Lstat(linkPath)
				switch {
				case statErr == nil:
					isSymlink := info.Mode()&os.ModeSymlink != 0
					// Replace dangling symlinks without --force.
					if isSymlink {
						target, readErr := os.Readlink(linkPath)

						dangling := readErr != nil
						if !dangling {
							_, statErr2 := os.Stat(linkPath)
							dangling = statErr2 != nil
						}

						if dangling || force {
							if err := os.Remove(linkPath); err != nil {
								_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fail: %s remove symlink: %v\n", linkName, err) //nolint:errcheck
								continue
							}

							if dangling {
								_, _ = fmt.Fprintf(cmd.OutOrStdout(), "replacing dangling symlink: %s (was -> %s)\n", linkName, target) //nolint:errcheck
							}
						} else if !force {
							_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skip: %s already exists\n", linkName) //nolint:errcheck
							continue
						}
					} else {
						_, _ = fmt.Fprintf(cmd.OutOrStdout(), "skip: %s exists and is not a symlink\n", linkName) //nolint:errcheck
						continue
					}
				case os.IsNotExist(statErr):
				default:
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fail: %s stat failed: %v\n", linkName, statErr) //nolint:errcheck
					continue
				}

				if err := os.Symlink(exeBase, linkPath); err != nil {
					_, _ = fmt.Fprintf(cmd.OutOrStdout(), "fail: %s create symlink: %v\n", linkName, err) //nolint:errcheck
					continue
				}

				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "created: %s -> %s\n", linkName, exeBase) //nolint:errcheck
			}

			return nil
		},
	}
	createCmd.Flags().BoolVarP(&force, "force", "f", false, "Replace existing symlink targets (non-symlink files are still skipped)")
	root.AddCommand(createCmd)

	return root
}
