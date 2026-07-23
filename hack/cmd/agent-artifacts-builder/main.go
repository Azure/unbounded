// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/Masterminds/semver/v3"
	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/registry"

	"github.com/Azure/unbounded/hack/cmd/agent-artifacts-builder/artifacts"
	"github.com/Azure/unbounded/internal/logger"
)

//go:embed kubernetes-versions.txt rootfs-images.txt
var defaultPublishInputsFS embed.FS

func main() {
	if err := newRootCommand().ExecuteContext(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	var (
		opts      artifacts.Options
		debug     bool
		logFormat string
	)

	cmd := &cobra.Command{
		Use:          "agent-artifacts-builder",
		Short:        "Build offline agent bootstrap artifact bundles",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			log := newLogger(debug, logFormat)

			if opts.ManifestPath == "" && opts.KubernetesVersion == "" {
				return fmt.Errorf("one of --manifest or --kubernetes-version is required")
			}

			if opts.ManifestPath != "" && opts.KubernetesVersion != "" {
				return fmt.Errorf("--manifest and --kubernetes-version are mutually exclusive")
			}

			return artifacts.Build(cmd.Context(), log, opts)
		},
	}

	cmd.PersistentFlags().BoolVar(&debug, "debug", false, "enable debug-level logging")
	cmd.PersistentFlags().StringVar(&logFormat, "log-format", "text", "log format: text or json")

	flags := cmd.Flags()
	flags.StringVar(&opts.OutputDir, "output-dir", "", "Directory where the offline artifact filesystem layout is written")
	flags.StringVar(&opts.ArchivePath, "archive", "", "Optional path for a gzip-compressed tar archive of the offline artifact bundle")
	flags.StringVar(&opts.OCIRef, "oci-ref", "", "Optional OCI artifact reference to push, with or without oci:// prefix")
	flags.StringVar(&opts.ManifestPath, "manifest", "", "Path to offline artifact manifest.json declaring artifact versions")
	flags.StringVar(&opts.KubernetesVersion, "kubernetes-version", "", "Kubernetes version for a default manifest using the agent's pinned runtime versions")
	flags.StringSliceVar(&opts.Architectures, "arch", nil, "Target architecture to include. Repeat or comma separate. Defaults to the host GOARCH")

	cmd.AddCommand(
		newBuildCommand(&debug, &logFormat),
		newResolvePublishInputsCommand(),
		newPublishVersionGroupCommand(&debug, &logFormat),
		newValidateOCICommand(&debug, &logFormat),
	)

	return cmd
}

func newBuildCommand(debug *bool, logFormat *string) *cobra.Command {
	var (
		outputDir          string
		kubernetesVersions []string
		rootfsImages       []string
	)

	cmd := &cobra.Command{
		Use:          "build",
		Short:        "Build the default upload-ready bootstrap artifact layout",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if outputDir == "" {
				return fmt.Errorf("--output-dir is required")
			}

			versions, err := resolveBuildKubernetesVersions(kubernetesVersions)
			if err != nil {
				return err
			}

			images, err := resolveBuildRootfsImages(rootfsImages)
			if err != nil {
				return err
			}

			return buildReleaseLayout(cmd.Context(), newLogger(*debug, *logFormat), outputDir, versions, images)
		},
	}

	cmd.Flags().StringVar(&outputDir, "output-dir", "", "Directory for rootfs and bootstrap artifact archives")
	cmd.Flags().StringArrayVar(&kubernetesVersions, "kubernetes-version", nil, "Kubernetes version to build. Repeat for multiple versions. Defaults to embedded versions")
	cmd.Flags().StringArrayVar(&rootfsImages, "rootfs-image", nil, "Tagged rootfs OCI image to build. Repeat for multiple images. Defaults to embedded images")

	return cmd
}

func resolveBuildKubernetesVersions(versions []string) ([]string, error) {
	if len(versions) == 0 {
		versionsRaw, err := defaultKubernetesVersions()
		if err != nil {
			return nil, err
		}

		versions = []string{versionsRaw}
	}

	resolved := normalizeKubernetesVersions(strings.Join(versions, "\n"))
	if len(resolved) == 0 {
		return nil, fmt.Errorf("at least one Kubernetes version is required")
	}

	return resolved, nil
}

func resolveBuildRootfsImages(images []string) ([]string, error) {
	if len(images) == 0 {
		imagesRaw, err := defaultRootfsImages()
		if err != nil {
			return nil, err
		}

		images = []string{imagesRaw}
	}

	return normalizeRootfsImages(strings.Join(images, "\n"))
}

func buildReleaseLayout(ctx context.Context, log *slog.Logger, outputDir string, versions, rootfsImages []string) error {
	rootfsDir := filepath.Join(outputDir, "rootfs")
	bootstrapDir := filepath.Join(outputDir, "bootstrap-artifacts")

	if err := os.MkdirAll(rootfsDir, 0o755); err != nil {
		return fmt.Errorf("create rootfs archive output directory: %w", err)
	}

	if err := os.MkdirAll(bootstrapDir, 0o755); err != nil {
		return fmt.Errorf("create bootstrap archive output directory: %w", err)
	}

	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error {
		return buildRootfsArchives(ctx, log, rootfsDir, rootfsImages)
	})
	group.Go(func() error {
		return buildBootstrapArchives(ctx, log, bootstrapDir, versions)
	})

	return group.Wait()
}

type rootfsArchivePlan struct {
	source      string
	archivePath string
}

func buildRootfsArchives(ctx context.Context, log *slog.Logger, outputDir string, images []string) error {
	plans, err := planRootfsArchives(outputDir, images)
	if err != nil {
		return err
	}

	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(3)

	for _, plan := range plans {
		group.Go(func() error {
			log.Info("building rootfs OCI layout archive", slog.String("source", plan.source), slog.String("archive", plan.archivePath))

			return artifacts.ArchiveOCIImage(ctx, plan.source, plan.archivePath)
		})
	}

	return group.Wait()
}

func planRootfsArchives(outputDir string, images []string) ([]rootfsArchivePlan, error) {
	plans := make([]rootfsArchivePlan, 0, len(images))
	sourcesByName := make(map[string]string, len(images))

	for _, image := range images {
		archiveName, err := artifacts.OCIImageArchiveName(image)
		if err != nil {
			return nil, err
		}

		if existingSource, ok := sourcesByName[archiveName]; ok {
			return nil, fmt.Errorf("rootfs images %q and %q produce the same archive name %q", existingSource, image, archiveName)
		}

		sourcesByName[archiveName] = image
		plans = append(plans, rootfsArchivePlan{
			source:      image,
			archivePath: filepath.Join(outputDir, archiveName),
		})
	}

	return plans, nil
}

const bootstrapArchiveConcurrency = 3

type bootstrapArchiveTask struct {
	bundleDir   string
	archivePath string
	cleanup     func()
}

func buildBootstrapArchives(ctx context.Context, log *slog.Logger, outputDir string, versions []string) error {
	stagingDir, cleanup, err := artifacts.NewStagingDir()
	if err != nil {
		return err
	}
	defer cleanup()

	return runBootstrapArchivePipeline(
		ctx,
		versions,
		func(ctx context.Context, version string) (bootstrapArchiveTask, error) {
			return prepareBootstrapArchive(ctx, log, stagingDir, outputDir, version)
		},
		func(task bootstrapArchiveTask) error {
			return artifacts.WriteBundleArchive(task.bundleDir, task.archivePath)
		},
	)
}

func runBootstrapArchivePipeline(
	ctx context.Context,
	versions []string,
	prepare func(context.Context, string) (bootstrapArchiveTask, error),
	archive func(bootstrapArchiveTask) error,
) error {
	tasks := make(chan bootstrapArchiveTask)
	group, ctx := errgroup.WithContext(ctx)

	group.Go(func() error {
		defer close(tasks)

		for _, version := range versions {
			task, err := prepare(ctx, version)
			if err != nil {
				return err
			}

			select {
			case tasks <- task:
			case <-ctx.Done():
				task.cleanup()

				return context.Cause(ctx)
			}
		}

		return nil
	})

	for range bootstrapArchiveConcurrency {
		group.Go(func() error {
			for {
				select {
				case task, ok := <-tasks:
					if !ok {
						return nil
					}

					err := archive(task)
					task.cleanup()

					if err != nil {
						return err
					}
				case <-ctx.Done():
					return nil
				}
			}
		})
	}

	return group.Wait()
}

func prepareBootstrapArchive(ctx context.Context, log *slog.Logger, stagingDir, outputDir, version string) (bootstrapArchiveTask, error) {
	bundleDir, err := os.MkdirTemp("", "unbounded-bootstrap-artifacts-*")
	if err != nil {
		return bootstrapArchiveTask{}, fmt.Errorf("create temporary bootstrap bundle directory: %w", err)
	}

	task := bootstrapArchiveTask{
		bundleDir:   bundleDir,
		archivePath: filepath.Join(outputDir, fmt.Sprintf("bootstrap-artifacts-k8s-%s.tar.gz", version)),
		cleanup: func() {
			os.RemoveAll(bundleDir) //nolint:errcheck // best effort cleanup
		},
	}

	log.Info("building bootstrap artifact archive", slog.String("archive", task.archivePath), slog.String("kubernetes_version", version))

	if err := artifacts.Build(ctx, log, artifacts.Options{
		OutputDir:         bundleDir,
		StagingDir:        stagingDir,
		KubernetesVersion: version,
		Architectures:     []string{"amd64", "arm64"},
	}); err != nil {
		task.cleanup()

		return bootstrapArchiveTask{}, err
	}

	if err := artifacts.ValidateBundle(log, bundleDir); err != nil {
		task.cleanup()

		return bootstrapArchiveTask{}, err
	}

	return task, nil
}

func newLogger(debug bool, format string) *slog.Logger {
	var lvl slog.LevelVar
	if debug {
		lvl.Set(slog.LevelDebug)
	}

	if strings.EqualFold(format, "json") {
		return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: &lvl}))
	}

	return slog.New(logger.NewPrettyFieldHandler(&lvl, logger.PrettyFieldHandlerOptions{
		AttrOrder: []string{"artifact", "source", "archive", "oci_ref", "digest", "staging_dir"},
	}))
}

// GitHub Actions publishing flow:
//
//  1. The resolve job runs resolve-publish-inputs once. It resolves the OCI tag
//     prefix, release tag, Kubernetes versions, and rootfs images from reusable
//     workflow or workflow_dispatch inputs, then writes GitHub outputs.
//  2. The main release workflow passes its version tag as both the OCI tag
//     prefix and GitHub release tag, with embedded default version and image lists.
//  3. Manual workflow_dispatch runs may pass explicit tags and comma, space, or
//     newline separated Kubernetes version and rootfs image lists. Missing
//     values use the short commit SHA or embedded defaults as appropriate.
//  4. Versions are grouped by Kubernetes minor, for example 1.34 and 1.35. The
//     workflow creates one publish job per minor group, while this binary still
//     publishes every patch version inside that group. Each publish job creates
//     one temporary staging directory so artifacts with the same version, such as
//     containerd or runc, are downloaded once and materialized into each bundle.
//  5. Each bundle is validated before publishing so invalid local content fails
//     the job before creating the HTTPS archive or pushing to the registry. The
//     archive is a gzip-compressed filesystem bundle, while the OCI tag is an
//     index with one platform manifest per target architecture.
//  6. Each published OCI tag must keep the shape documented in
//     designs/agent-offline-bootstrap.md:
//     ghcr.io/<owner>/unbounded/bootstrap-artifacts:<tag-prefix>-k8s-<kubernetes-version>
func newPublishVersionGroupCommand(debug *bool, logFormat *string) *cobra.Command {
	return &cobra.Command{
		Use:          "publish-version-group",
		Short:        "Build and publish a Kubernetes minor version group from GitHub Actions inputs",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return publishVersionGroup(cmd.Context(), newLogger(*debug, *logFormat))
		},
	}
}

func publishVersionGroup(ctx context.Context, log *slog.Logger) error {
	registry := strings.TrimRight(strings.ToLower(strings.TrimSpace(os.Getenv("REGISTRY"))), "/")
	if registry == "" {
		return fmt.Errorf("REGISTRY is required")
	}

	tagPrefix := strings.TrimSpace(os.Getenv("ARTIFACT_TAG_PREFIX"))
	if tagPrefix == "" {
		return fmt.Errorf("ARTIFACT_TAG_PREFIX is required")
	}

	versions, err := kubernetesVersionsFromJSON(os.Getenv("KUBERNETES_VERSIONS_JSON"))
	if err != nil {
		return err
	}

	stagingDir, cleanup, err := artifacts.NewStagingDir()
	if err != nil {
		return err
	}
	defer cleanup()

	log.Info("using offline artifact staging directory", slog.String("staging_dir", stagingDir))

	for _, kubernetesVersion := range versions {
		ociRef := fmt.Sprintf("%s/bootstrap-artifacts:%s-k8s-%s", registry, tagPrefix, kubernetesVersion)
		outputDir := filepath.Join("dist", "bootstrap-artifacts", kubernetesVersion)
		archivePath := filepath.Join("dist", "bootstrap-artifacts", fmt.Sprintf("bootstrap-artifacts-%s-k8s-%s.tar.gz", tagPrefix, kubernetesVersion))

		log.Info("building offline artifact bundle",
			slog.String("kubernetes_version", kubernetesVersion),
			slog.String("output_dir", outputDir),
			slog.String("oci_ref", ociRef),
		)

		if err := artifacts.Build(ctx, log, artifacts.Options{
			OutputDir:         outputDir,
			StagingDir:        stagingDir,
			KubernetesVersion: kubernetesVersion,
			Architectures:     []string{"amd64", "arm64"},
			ArchivePath:       archivePath,
			OCIRef:            ociRef,
		}); err != nil {
			return err
		}

		if err := logBundleContents(log, outputDir); err != nil {
			return err
		}

		log.Info("published offline artifact bundle",
			slog.String("archive", archivePath),
			slog.String("oci_ref", ociRef),
		)
	}

	return nil
}

func kubernetesVersionsFromJSON(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("KUBERNETES_VERSIONS_JSON is required")
	}

	var versions []string
	if err := json.Unmarshal([]byte(raw), &versions); err != nil {
		return nil, fmt.Errorf("parse KUBERNETES_VERSIONS_JSON: %w", err)
	}

	if len(versions) == 0 {
		return nil, fmt.Errorf("KUBERNETES_VERSIONS_JSON must contain at least one version")
	}

	return versions, nil
}

func logBundleContents(log *slog.Logger, root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if entry.IsDir() {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		log.Info("offline artifact bundle file",
			slog.String("artifact", filepath.ToSlash(rel)),
			slog.Int64("bytes", info.Size()),
		)

		return nil
	})
}

func newValidateOCICommand(debug *bool, logFormat *string) *cobra.Command {
	var ociRef string

	cmd := &cobra.Command{
		Use:          "validate-oci",
		Short:        "Pull and validate a pushed offline artifact OCI bundle",
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if ociRef == "" {
				return fmt.Errorf("--oci-ref is required")
			}

			return artifacts.ValidateOCI(cmd.Context(), newLogger(*debug, *logFormat), ociRef)
		},
	}

	flags := cmd.Flags()
	flags.StringVar(&ociRef, "oci-ref", "", "OCI artifact reference to pull and validate, with or without oci:// prefix")

	return cmd
}

func newResolvePublishInputsCommand() *cobra.Command {
	return &cobra.Command{
		Use:          "resolve-publish-inputs",
		Short:        "Resolve GitHub Actions inputs for publishing offline artifact bundles",
		SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error {
			return resolvePublishInputs()
		},
	}
}

func resolvePublishInputs() error {
	eventName := os.Getenv("EVENT_NAME")
	refName := os.Getenv("REF_NAME")
	inputTag := strings.TrimSpace(os.Getenv("INPUT_TAG"))
	inputReleaseTag := strings.TrimSpace(os.Getenv("INPUT_RELEASE_TAG"))
	inputVersions := strings.TrimSpace(os.Getenv("INPUT_KUBERNETES_VERSIONS"))
	inputRootfsImages := strings.TrimSpace(os.Getenv("INPUT_ROOTFS_IMAGES"))
	githubSHA := os.Getenv("GITHUB_SHA_VALUE")

	var (
		tag             string
		releaseTag      string
		versionsRaw     string
		rootfsImagesRaw string
	)
	if eventName == "push" {
		tag = strings.TrimSpace(refName)
		releaseTag = tag

		var err error

		versionsRaw, err = defaultKubernetesVersions()
		if err != nil {
			return err
		}

		rootfsImagesRaw, err = defaultRootfsImages()
		if err != nil {
			return err
		}
	} else {
		tag = inputTag
		releaseTag = inputReleaseTag

		if tag == "" {
			tag = shortSHA(githubSHA)
		}

		versionsRaw = inputVersions
		if versionsRaw == "" {
			var err error

			versionsRaw, err = defaultKubernetesVersions()
			if err != nil {
				return err
			}
		}

		rootfsImagesRaw = inputRootfsImages
		if rootfsImagesRaw == "" {
			var err error

			rootfsImagesRaw, err = defaultRootfsImages()
			if err != nil {
				return err
			}
		}
	}

	if tag == "" {
		return fmt.Errorf("artifact tag could not be resolved")
	}

	versions := normalizeKubernetesVersions(versionsRaw)
	if len(versions) == 0 {
		return fmt.Errorf("at least one Kubernetes version is required")
	}

	versionsJSON, err := json.Marshal(versions)
	if err != nil {
		return fmt.Errorf("marshal Kubernetes versions: %w", err)
	}

	rootfsImages, err := normalizeRootfsImages(rootfsImagesRaw)
	if err != nil {
		return err
	}

	rootfsImagesJSON, err := json.Marshal(rootfsImages)
	if err != nil {
		return fmt.Errorf("marshal rootfs images: %w", err)
	}

	groups, err := groupKubernetesVersionsByMinor(versions)
	if err != nil {
		return err
	}

	groupsJSON, err := json.Marshal(groups)
	if err != nil {
		return fmt.Errorf("marshal Kubernetes version groups: %w", err)
	}

	if err := writeGitHubOutput(map[string]string{
		"tag":                       tag,
		"release_tag":               releaseTag,
		"kubernetes_versions":       string(versionsJSON),
		"kubernetes_version_groups": string(groupsJSON),
		"rootfs_images":             string(rootfsImagesJSON),
	}); err != nil {
		return err
	}

	fmt.Printf("Publishing tag prefix: %s\n", tag)
	fmt.Printf("GitHub release tag: %s\n", releaseTag)
	fmt.Printf("Kubernetes versions: %s\n", versionsJSON)
	fmt.Printf("Kubernetes version groups: %s\n", groupsJSON)
	fmt.Printf("Rootfs images: %s\n", rootfsImagesJSON)

	return nil
}

func defaultKubernetesVersions() (string, error) {
	path := strings.TrimSpace(os.Getenv("DEFAULT_KUBERNETES_VERSIONS_FILE"))
	if path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return "", fmt.Errorf("read default Kubernetes versions file %q: %w", path, err)
		}

		return stripLineComments(string(data)), nil
	}

	data, err := defaultPublishInputsFS.ReadFile("kubernetes-versions.txt")
	if err != nil {
		return "", fmt.Errorf("read embedded default Kubernetes versions: %w", err)
	}

	return stripLineComments(string(data)), nil
}

func defaultRootfsImages() (string, error) {
	path := strings.TrimSpace(os.Getenv("DEFAULT_ROOTFS_IMAGES_FILE"))
	if path != "" {
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return "", fmt.Errorf("read default rootfs images file %q: %w", path, err)
		}

		return stripLineComments(string(data)), nil
	}

	data, err := defaultPublishInputsFS.ReadFile("rootfs-images.txt")
	if err != nil {
		return "", fmt.Errorf("read embedded default rootfs images: %w", err)
	}

	return stripLineComments(string(data)), nil
}

func stripLineComments(raw string) string {
	var out []string

	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "\n")
}

func normalizeRootfsImages(raw string) ([]string, error) {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})

	images := make([]string, 0, len(fields))
	for _, field := range fields {
		image := strings.TrimSpace(field)
		if image == "" {
			continue
		}

		ref, err := registry.ParseReference(strings.TrimPrefix(image, "oci://"))
		if err != nil {
			return nil, fmt.Errorf("parse rootfs image %q: %w", image, err)
		}

		if err := ref.ValidateReferenceAsTag(); err != nil {
			return nil, fmt.Errorf("rootfs image %q must use a tag: %w", image, err)
		}

		images = append(images, image)
	}

	if len(images) == 0 {
		return nil, fmt.Errorf("at least one rootfs image is required")
	}

	return images, nil
}

func normalizeKubernetesVersions(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	})

	versions := make([]string, 0, len(fields))
	for _, field := range fields {
		version := strings.TrimSpace(field)
		if version == "" {
			continue
		}

		if !strings.HasPrefix(version, "v") {
			version = "v" + version
		}

		versions = append(versions, version)
	}

	return versions
}

type kubernetesVersionGroup struct {
	Minor    string   `json:"minor"`
	Versions []string `json:"versions"`
}

func groupKubernetesVersionsByMinor(versions []string) ([]kubernetesVersionGroup, error) {
	groups := []kubernetesVersionGroup{}
	indexByMinor := map[string]int{}

	for _, version := range versions {
		minor, err := kubernetesMinorVersion(version)
		if err != nil {
			return nil, err
		}

		index, ok := indexByMinor[minor]
		if !ok {
			index = len(groups)
			indexByMinor[minor] = index
			groups = append(groups, kubernetesVersionGroup{Minor: minor})
		}

		groups[index].Versions = append(groups[index].Versions, version)
	}

	return groups, nil
}

func kubernetesMinorVersion(version string) (string, error) {
	parsed, err := semver.NewVersion(strings.TrimSpace(version))
	if err != nil {
		return "", fmt.Errorf("parse Kubernetes version %q: %w", version, err)
	}

	return fmt.Sprintf("%d.%d", parsed.Major(), parsed.Minor()), nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}

	return sha[:12]
}

func writeGitHubOutput(values map[string]string) (err error) {
	outputPath := os.Getenv("GITHUB_OUTPUT")
	if outputPath == "" {
		for key, value := range values {
			fmt.Printf("%s=%s\n", key, value)
		}

		return nil
	}

	file, err := os.OpenFile(outputPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open GITHUB_OUTPUT: %w", err)
	}

	defer func() {
		if closeErr := file.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close GITHUB_OUTPUT: %w", closeErr)
		}
	}()

	for key, value := range values {
		if _, err := fmt.Fprintf(file, "%s=%s\n", key, value); err != nil {
			return fmt.Errorf("write GITHUB_OUTPUT: %w", err)
		}
	}

	return nil
}
