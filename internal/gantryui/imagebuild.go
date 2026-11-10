// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/distribution/reference"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	"oras.land/oras-go/v2/registry/remote"
)

const (
	MaxBuildLayers    = 100
	MaxBuildMB        = 100_000
	ImageBuildTimeout = 6 * time.Hour
	payloadMB         = int64(1_000_000)
	buildLabel        = "gantry.unbounded-cloud.io/build"
	buildAnnotation   = "gantry.unbounded-cloud.io/build-request"
)

type BuildRequest struct {
	Layers  int `json:"layers"`
	LayerMB int `json:"layerMB"`
}

func (request BuildRequest) Validate() error {
	if request.Layers < 1 || request.Layers > MaxBuildLayers {
		return fmt.Errorf("payload layers must be between 1 and %d", MaxBuildLayers)
	}

	if request.LayerMB < 1 || request.LayerMB > MaxBuildMB ||
		request.Layers > MaxBuildMB/request.LayerMB {
		return fmt.Errorf("total random payload must be between 1 and %d MB", MaxBuildMB)
	}

	return nil
}

type BuildSpec struct {
	BuildRequest
	Name         string `json:"name"`
	Registry     string `json:"registry"`
	Repository   string `json:"repository"`
	Base         string `json:"base"`
	Namespace    string `json:"namespace"`
	NodePool     string `json:"nodePool"`
	Architecture string `json:"architecture"`
}

type BuildProgress struct {
	Stage      string `json:"stage"`
	Layer      int    `json:"layer"`
	Layers     int    `json:"layers"`
	Bytes      int64  `json:"bytes"`
	LayerBytes int64  `json:"layerBytes"`
	Message    string `json:"message"`
}

// RunImageBuild streams one temporary layer at a time, then starts the
// digest-pinned workload only after the registry has accepted the image.
func RunImageBuild(ctx context.Context, client kubernetes.Interface, registry *Registry, spec BuildSpec, directory string, emit func(BuildProgress)) (retErr error) {
	defer func() {
		if retErr != nil {
			retErr = registryRequestError{err: retErr}
			emit(BuildProgress{Stage: "failed", Layers: spec.Layers, Message: retErr.Error()})
		}
	}()

	if err := spec.Validate(); err != nil {
		return err
	}

	if !buildName.MatchString(spec.Name) || registry.host != spec.Registry || registry.architecture != spec.Architecture {
		return errors.New("invalid build identity or registry configuration")
	}

	if len(validation.IsDNS1123Label(spec.Namespace)) > 0 || spec.NodePool == "" || len(validation.IsValidLabelValue(spec.NodePool)) > 0 {
		return errors.New("valid demo namespace and worker pool are required")
	}

	if _, err := reference.WithName(spec.Registry + "/" + spec.Repository); err != nil {
		return fmt.Errorf("invalid build repository: %w", err)
	}

	base := strings.Split(spec.Base, "@")
	if len(base) != 2 || !strings.HasPrefix(base[0], spec.Registry+"/") {
		return errors.New("build base must be digest-pinned in the configured ACR")
	}

	if d, err := godigest.Parse(base[1]); err != nil || d.Algorithm() != godigest.SHA256 {
		return errors.New("build base must use a valid sha256 digest")
	}

	baseRepository := strings.TrimPrefix(base[0], spec.Registry+"/")

	body, err := registry.get(ctx, "/v2/"+baseRepository+"/manifests/"+base[1], nil)
	if err != nil {
		return fmt.Errorf("read runtime base: %w", err)
	}

	if godigest.FromBytes(body).String() != base[1] {
		return errors.New("runtime base manifest does not match its pinned digest")
	}

	var manifest ocispec.Manifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return err
	}

	if manifest.Config.Digest == "" || len(manifest.Layers) == 0 || len(manifest.Layers)+spec.Layers > 512 {
		return errors.New("runtime base must be a single-platform image with at most 512 total layers")
	}

	body, err = registry.get(ctx, "/v2/"+baseRepository+"/blobs/"+manifest.Config.Digest.String(), nil)
	if err != nil {
		return err
	}

	if godigest.FromBytes(body) != manifest.Config.Digest {
		return errors.New("runtime base configuration does not match its descriptor")
	}

	var config ocispec.Image
	if err := json.Unmarshal(body, &config); err != nil {
		return err
	}

	if config.OS != "linux" || config.Architecture != spec.Architecture ||
		len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return errors.New("runtime base does not match the requested Linux architecture")
	}

	source, err := remote.NewRepository(base[0])
	if err != nil {
		return err
	}

	target, err := remote.NewRepository(spec.Registry + "/" + spec.Repository)
	if err != nil {
		return err
	}
	// Uploads can exceed the metadata client's timeout. The Job's context
	// bounds the operation without imposing a short whole-upload deadline.
	uploadClient := *registry.client
	httpClient := *registry.client.Client
	httpClient.Timeout = 0
	uploadClient.Client = &httpClient
	source.Client, target.Client = &uploadClient, &uploadClient

	emit(BuildProgress{Stage: "base", Layers: spec.Layers, Message: "Copying the small runtime base"})

	for _, layer := range manifest.Layers {
		if err := copyBuildLayer(ctx, source, target, layer); err != nil {
			return fmt.Errorf("copy runtime layer: %w", err)
		}
	}

	for index := 1; index <= spec.Layers; index++ {
		layer, diffID, err := pushRandomLayer(ctx, target, directory, index, spec, emit)
		if err != nil {
			return fmt.Errorf("payload layer %d: %w", index, err)
		}

		manifest.Layers = append(manifest.Layers, layer)

		config.RootFS.DiffIDs = append(config.RootFS.DiffIDs, diffID)
		if len(config.History) > 0 {
			config.History = append(config.History, ocispec.History{CreatedBy: "Gantry random payload"})
		}
	}

	now := time.Now().UTC()
	config.Created = &now
	config.Config.Entrypoint = nil
	config.Config.Cmd = []string{"/bin/sh", "-c", "while :; do sleep 3600; done"}
	config.Config.User = ""

	body, err = json.Marshal(config)
	if err != nil {
		return err
	}

	manifest.Config = buildDescriptor(ocispec.MediaTypeImageConfig, body)
	if err := target.Push(ctx, manifest.Config, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("push image configuration: %w", err)
	}

	manifest.MediaType = ocispec.MediaTypeImageManifest
	manifest.Subject, manifest.ArtifactType = nil, ""

	body, err = json.Marshal(manifest)
	if err != nil {
		return err
	}

	emit(BuildProgress{Stage: "publishing", Layers: spec.Layers, Message: "Publishing the completed OCI image"})

	if err := target.PushReference(ctx, buildDescriptor(ocispec.MediaTypeImageManifest, body), bytes.NewReader(body), spec.Name); err != nil {
		return fmt.Errorf("publish image manifest: %w", err)
	}

	image, err := registry.image(ctx, spec.Repository, spec.Name)
	if err != nil {
		return fmt.Errorf("verify published image: %w", err)
	}

	if image == nil || image.Digest != godigest.FromBytes(body).String() {
		return errors.New("published image did not match the generated manifest")
	}

	emit(BuildProgress{Stage: "deploying", Layers: spec.Layers, Message: "Starting the Flex-worker pull"})

	workload, err := buildDeployment(spec.Namespace, spec.NodePool, *image)
	if err != nil {
		return err
	}

	workload.Labels[buildLabel] = spec.Name
	if _, err := client.AppsV1().DaemonSets(spec.Namespace).Create(ctx, workload, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("create generated-image workload: %w", err)
	}

	emit(BuildProgress{Stage: "complete", Layers: spec.Layers, Message: image.ID})

	return nil
}

func buildDescriptor(mediaType string, body []byte) ocispec.Descriptor {
	return ocispec.Descriptor{MediaType: mediaType, Digest: godigest.FromBytes(body), Size: int64(len(body))}
}

func copyBuildLayer(ctx context.Context, source, target *remote.Repository, layer ocispec.Descriptor) (retErr error) {
	reader, err := source.Fetch(ctx, layer)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, reader.Close()) }()

	return target.Push(ctx, layer, reader)
}

type buildProgressReader struct {
	ctx    context.Context
	reader io.Reader
	total  int64
	last   time.Time
	report func(int64)
}

func (reader *buildProgressReader) Read(buffer []byte) (int, error) {
	if err := reader.ctx.Err(); err != nil {
		return 0, err
	}

	n, err := reader.reader.Read(buffer)

	reader.total += int64(n)
	if time.Since(reader.last) >= time.Second || errors.Is(err, io.EOF) {
		reader.last = time.Now()
		reader.report(reader.total)
	}

	return n, err
}

func pushRandomLayer(ctx context.Context, target *remote.Repository, directory string, index int, spec BuildSpec, emit func(BuildProgress)) (layer ocispec.Descriptor, diffID godigest.Digest, retErr error) {
	file, err := os.CreateTemp(directory, "gantry-layer-*")
	if err != nil {
		return layer, diffID, err
	}
	defer func() { retErr = errors.Join(retErr, file.Close(), os.Remove(file.Name())) }()

	report := func(stage string, total int64) func(int64) {
		return func(n int64) {
			emit(BuildProgress{Stage: stage, Layer: index, Layers: spec.Layers, Bytes: n, LayerBytes: total})
		}
	}
	size := int64(spec.LayerMB) * payloadMB

	layer, diffID, err = writeRandomLayer(ctx, file, index, size, rand.Reader, report("generating", size))
	if err != nil {
		return layer, diffID, err
	}

	end, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		return layer, diffID, err
	}

	layer.Size = end
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return layer, diffID, err
	}

	reader := &buildProgressReader{ctx: ctx, reader: file, report: report("pushing", layer.Size)}
	if err := target.Push(ctx, layer, reader); err != nil {
		return layer, diffID, err
	}

	return layer, diffID, nil
}

func writeRandomLayer(ctx context.Context, output io.Writer, index int, size int64, source io.Reader, report func(int64)) (ocispec.Descriptor, godigest.Digest, error) {
	compressed, uncompressed := sha256.New(), sha256.New()

	gz, err := gzip.NewWriterLevel(io.MultiWriter(output, compressed), gzip.NoCompression)
	if err != nil {
		return ocispec.Descriptor{}, "", err
	}

	archive := tar.NewWriter(io.MultiWriter(gz, uncompressed))
	if err := archive.WriteHeader(&tar.Header{Name: fmt.Sprintf("payload/layer-%02d.bin", index), Mode: 0o644, Size: size}); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	reader := &buildProgressReader{ctx: ctx, reader: io.LimitReader(source, size), report: report}
	if _, err := io.CopyN(archive, reader, size); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	if err := archive.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	if err := gz.Close(); err != nil {
		return ocispec.Descriptor{}, "", err
	}

	report(size)

	return ocispec.Descriptor{MediaType: ocispec.MediaTypeImageLayerGzip, Digest: godigest.NewDigest(godigest.SHA256, compressed)}, godigest.NewDigest(godigest.SHA256, uncompressed), nil
}
