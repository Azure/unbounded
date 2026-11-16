// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package netboot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

// blockMediaBuild stops the runner after authorization and before publication.
func blockMediaBuild(t *testing.T, p *MediaProvider) chan struct{} {
	t.Helper()

	var calls []string

	run := mockMediaRunner(t, &calls)
	resume := make(chan struct{})
	p.Run = func(ctx context.Context, name string, args ...string) error {
		if name == "xorriso" {
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		return run(ctx, name, args...)
	}

	return resume
}

func mediaResult(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	default:
		t.Fatal("media operation is still blocked")
		return nil
	}
}

func TestMediaBuildDoesNotBlockOtherIDs(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, machine, op := mediaFixture(t, "amd64")
		id := op.Status.Targets[0].BootMedia.ID
		resume := blockMediaBuild(t, p)
		close(resume)

		file, err := p.openISO(t.Context(), "ub-"+id+".iso", true)
		require.NoError(t, err)
		require.NoError(t, file.Close())

		other := op.DeepCopy()
		other.Name, other.UID, other.ResourceVersion = "other", "other-uid", ""
		otherID := strings.Repeat("d", 32)
		other.Status.Targets[0].BootMedia.ID = otherID
		require.NoError(t, p.Client.Create(t.Context(), other))
		resume = blockMediaBuild(t, p)
		build := make(chan error, 1)

		go func() {
			_, err := p.Prepare(t.Context(), machine, other, otherID)
			build <- err
		}()

		synctest.Wait()
		require.Len(t, p.mediaLocks, 1)
		require.Contains(t, p.mediaLocks, otherID)

		cached := make(chan error, 1)
		w := httptest.NewRecorder()

		go func() {
			r := httptest.NewRequest(http.MethodGet, "/bootmedia/"+id+"/boot.iso", nil)
			r.Header.Set("Range", "bytes=0-8")
			p.ServeHTTP(w, r)

			cached <- nil
		}()

		opened := make(chan error, 1)

		go func() {
			file, err := p.OpenISO(t.Context(), "ub-"+id+".iso")
			if err == nil {
				err = file.Close()
			}

			opened <- err
		}()

		synctest.Wait()
		require.NoError(t, mediaResult(t, cached))
		require.Equal(t, http.StatusPartialContent, w.Code)
		require.Equal(t, "immutable", w.Body.String())
		require.NoError(t, mediaResult(t, opened))

		released := make(chan error, 1)
		go func() { released <- p.Release(t.Context(), id) }()

		synctest.Wait()
		require.NoError(t, mediaResult(t, released))
		close(resume)
		synctest.Wait()
		require.NoError(t, mediaResult(t, build))
		require.Empty(t, p.mediaLocks)
	})
}

func TestMediaSameIDWaitCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, machine, op := mediaFixture(t, "amd64")
		id := op.Status.Targets[0].BootMedia.ID
		resume := blockMediaBuild(t, p)
		build := make(chan error, 1)

		go func() {
			_, err := p.Prepare(t.Context(), machine, op, id)
			build <- err
		}()

		synctest.Wait()

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		waiters := []func() error{
			func() error { _, err := p.Prepare(ctx, machine, op, id); return err },
			func() error { _, err := p.OpenISO(ctx, "ub-"+id+".iso"); return err },
			func() error { _, err := p.openISO(ctx, "ub-"+id+".iso", true); return err },
			func() error { return p.Release(ctx, id) },
		}

		results := make(chan error, len(waiters))
		for _, wait := range waiters {
			go func() { results <- wait() }()
		}

		synctest.Wait()
		require.Equal(t, len(waiters)+1, p.mediaLocks[id].users)
		cancel()
		synctest.Wait()

		for range waiters {
			require.ErrorIs(t, mediaResult(t, results), context.Canceled)
		}

		require.Equal(t, 1, p.mediaLocks[id].users)
		close(resume)
		synctest.Wait()
		require.NoError(t, mediaResult(t, build))
		require.Empty(t, p.mediaLocks)

		// A canceled caller must not acquire even an uncontended lock.
		require.ErrorIs(t, p.Release(ctx, id), context.Canceled)
		require.Empty(t, p.mediaLocks)
		_, err := os.Stat(filepath.Join(p.CacheDir, "bootmedia", id))
		require.NoError(t, err)
	})
}

func TestMediaBuildWaitersReuseOrRetry(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "reuse completed build"
		if canceled {
			name = "retry canceled build"
		}

		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, machine, op := mediaFixture(t, "amd64")
				id := op.Status.Targets[0].BootMedia.ID
				resume := blockMediaBuild(t, p)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()

				build := make(chan error, 1)

				go func() {
					_, err := p.Prepare(ctx, machine, op, id)
					build <- err
				}()

				synctest.Wait()

				waiters := make(chan error, 2)

				go func() {
					_, err := p.Prepare(t.Context(), machine, op, id)
					waiters <- err
				}()
				go func() {
					file, err := p.openISO(t.Context(), "ub-"+id+".iso", true)
					if err == nil {
						err = file.Close()
					}

					waiters <- err
				}()

				synctest.Wait()
				require.Equal(t, 3, p.mediaLocks[id].users)

				if canceled {
					cancel()
					synctest.Wait()
					require.ErrorIs(t, mediaResult(t, build), context.Canceled)
					require.Equal(t, 2, p.mediaLocks[id].users)
				}

				close(resume)
				synctest.Wait()

				if !canceled {
					require.NoError(t, mediaResult(t, build))
				}

				for range 2 {
					require.NoError(t, mediaResult(t, waiters))
				}

				require.Empty(t, p.mediaLocks)
				require.Contains(t, p.grants, id)
				entries, err := os.ReadDir(filepath.Join(p.CacheDir, "bootmedia"))
				require.NoError(t, err)
				require.Len(t, entries, 1, "failed and duplicate builds leave no staging directories")
				require.Equal(t, id, entries[0].Name())
			})
		})
	}
}

func TestMediaReleaseOrdersWithBuild(t *testing.T) {
	for _, httpBuild := range []bool{false, true} {
		name := "prepare"
		if httpBuild {
			name = "HTTP grant"
		}

		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, machine, op := mediaFixture(t, "amd64")
				id := op.Status.Targets[0].BootMedia.ID
				resume := blockMediaBuild(t, p)
				build := make(chan error, 1)

				go func() {
					var err error

					if httpBuild {
						var file *os.File

						file, err = p.openISO(t.Context(), "ub-"+id+".iso", true)
						if err == nil {
							err = file.Close()
						}
					} else {
						_, err = p.Prepare(t.Context(), machine, op, id)
					}

					build <- err
				}()

				synctest.Wait()

				// Release follows durable revocation, but the build has an older owner.
				op.Status.Targets[0].BootMedia.Cleaned = true
				require.NoError(t, p.Client.Status().Update(t.Context(), op))

				released := make(chan error, 1)
				go func() { released <- p.Release(t.Context(), id) }()

				synctest.Wait()
				require.Empty(t, released)
				require.Equal(t, 2, p.mediaLocks[id].users)

				close(resume)
				synctest.Wait()
				require.NoError(t, mediaResult(t, build))
				require.NoError(t, mediaResult(t, released))
				require.Empty(t, p.grants)
				require.Empty(t, p.mediaLocks)
				_, err := os.Stat(filepath.Join(p.CacheDir, "bootmedia", id))
				require.ErrorIs(t, err, os.ErrNotExist)
				_, err = p.openISO(t.Context(), "ub-"+id+".iso", true)
				require.ErrorContains(t, err, "authorization ended")
				_, err = p.Prepare(t.Context(), machine, op, id)
				require.ErrorContains(t, err, "authorization ended")
				require.Empty(t, p.grants)
			})
		})
	}
}
