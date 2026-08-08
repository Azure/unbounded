// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifacts

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/images/archive"
	"github.com/containerd/containerd/v2/core/remotes"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	localcontent "github.com/containerd/containerd/v2/plugins/content/local"
	"github.com/containerd/platforms"
	"github.com/google/renameio/v2"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

func exportContainerImages(ctx context.Context, log *slog.Logger, rootDir string, imageArchives []ContainerImageArchive) error {
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(2)

	for _, image := range imageArchives {
		group.Go(func() error {
			return exportContainerImage(ctx, log, rootDir, image)
		})
	}

	return group.Wait()
}

func exportContainerImage(ctx context.Context, log *slog.Logger, rootDir string, image ContainerImageArchive) error {
	path := filepath.Join(rootDir, filepath.FromSlash(image.Path))
	if _, err := os.Stat(path); err == nil {
		log.Info("skipping existing container image archive", slog.String("artifact", image.Path), slog.String("source", image.ImageTag))
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat %q: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create dir for %q: %w", path, err)
	}

	log.Info("exporting container image archive", slog.String("artifact", image.Path), slog.String("source", image.ImageTag))

	if err := exportContainerImageArchive(ctx, image.ImageTag, image.Arch, path); err != nil {
		return err
	}

	if err := writeGeneratedChecksum(path); err != nil {
		return err
	}

	log.Info("exported container image archive", slog.String("artifact", image.Path), slog.String("source", image.ImageTag))

	return nil
}

func exportContainerImageArchive(ctx context.Context, imageTag, arch, dest string) (err error) {
	contentDir, cleanup, err := NewStagingDir()
	if err != nil {
		return err
	}
	defer cleanup()

	store, err := localcontent.NewStore(contentDir)
	if err != nil {
		return fmt.Errorf("create image content store: %w", err)
	}

	resolver := docker.NewResolver(docker.ResolverOptions{})

	name, desc, err := resolver.Resolve(ctx, imageTag)
	if err != nil {
		return fmt.Errorf("resolve container image %q: %w", imageTag, err)
	}

	fetcher, err := resolver.Fetcher(ctx, name)
	if err != nil {
		return fmt.Errorf("create fetcher for container image %q: %w", imageTag, err)
	}

	platform := platforms.OnlyStrict(ocispec.Platform{OS: "linux", Architecture: arch})
	childrenHandler := images.ChildrenHandler(store)
	childrenHandler = images.FilterPlatforms(childrenHandler, platform)
	childrenHandler = images.LimitManifests(childrenHandler, platform, 1)

	handler := images.Handlers(
		remotes.FetchHandler(store, fetcher),
		childrenHandler,
	)
	if err := images.Dispatch(ctx, handler, nil, desc); err != nil {
		return fmt.Errorf("pull container image %q for linux/%s: %w", imageTag, arch, err)
	}

	out, err := renameio.NewPendingFile(dest, renameio.WithPermissions(0o644))
	if err != nil {
		return err
	}
	defer out.Cleanup() //nolint:errcheck // pending file cleanup

	if err := archive.Export(ctx, store, out,
		archive.WithManifest(desc, imageTag),
		archive.WithPlatform(platform),
	); err != nil {
		return fmt.Errorf("export container image %q for linux/%s: %w", imageTag, arch, err)
	}

	if err := out.CloseAtomicallyReplace(); err != nil {
		return fmt.Errorf("install %q: %w", dest, err)
	}

	return nil
}
