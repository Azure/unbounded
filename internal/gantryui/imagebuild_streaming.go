// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"

	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/Azure/unbounded/internal/gantry/manifest"
)

const streamingArtifactType = "application/vnd.azure.artifact.streaming.v1"

var streamingOperationID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

type streamingOperation struct {
	ID       string          `json:"id"`
	Status   string          `json:"status"`
	Progress string          `json:"progress"`
	Details  string          `json:"details"`
	Error    json.RawMessage `json:"error"`
}

func (r *Registry) buildStreamingImage(ctx context.Context, target *remote.Repository, spec BuildSpec, source string, emit func(BuildProgress)) (*Image, error) {
	ctx = auth.AppendRepositoryScope(ctx, target.Reference, auth.ActionPull, auth.ActionPush)

	operation, err := r.startStreamingConversion(ctx, spec.Repository, source)
	if err != nil {
		return nil, err
	}

	if err := r.waitStreamingConversion(ctx, spec.Repository, operation, func(message string) {
		emit(BuildProgress{Stage: "converting", Layers: spec.Layers, Message: message})
	}); err != nil {
		return nil, err
	}

	body, err := r.get(ctx, "/v2/"+spec.Repository+"/referrers/"+source, url.Values{"artifactType": {streamingArtifactType}})
	if err != nil {
		return nil, fmt.Errorf("read streaming referrers: %w", err)
	}

	var index ocispec.Index
	if err := json.Unmarshal(body, &index); err != nil {
		return nil, fmt.Errorf("decode streaming referrers: %w", err)
	}

	var candidates []ocispec.Descriptor

	for _, descriptor := range index.Manifests {
		if descriptor.ArtifactType == streamingArtifactType {
			candidates = append(candidates, descriptor)
		}
	}

	if len(candidates) != 1 {
		return nil, fmt.Errorf("expected one streaming artifact, got %d", len(candidates))
	}

	descriptor := candidates[0]
	if err := descriptor.Digest.Validate(); err != nil || descriptor.Digest.Algorithm() != godigest.SHA256 {
		return nil, errors.New("streaming artifact has an invalid sha256 digest")
	}

	body, err = r.get(ctx, "/v2/"+spec.Repository+"/manifests/"+descriptor.Digest.String(), nil)
	if err != nil {
		return nil, err
	}

	var converted ocispec.Manifest
	if err := json.Unmarshal(body, &converted); err != nil {
		return nil, err
	}

	if int64(len(body)) != descriptor.Size || converted.Subject == nil || converted.Subject.Digest.String() != source ||
		converted.ArtifactType != streamingArtifactType || !manifest.IsOverlayBDStreamingManifest(body) {
		return nil, errors.New("converted artifact is not a streaming manifest for the generated source")
	}

	image, err := r.image(ctx, spec.Repository, descriptor.Digest.String())
	if err != nil {
		return nil, err
	}

	if image == nil || !image.Streaming || image.OS != "linux" || image.Architecture != spec.Architecture {
		return nil, errors.New("streaming artifact does not match the requested Linux architecture")
	}

	if err := target.PushReference(ctx, buildDescriptor(converted.MediaType, body), bytes.NewReader(body), spec.Name); err != nil {
		return nil, fmt.Errorf("tag streaming artifact: %w", err)
	}

	tagged, err := r.image(ctx, spec.Repository, spec.Name)
	if err != nil {
		return nil, err
	}

	if tagged == nil || !tagged.Streaming || tagged.Digest != descriptor.Digest.String() {
		return nil, errors.New("published streaming tag does not match the verified artifact")
	}

	return tagged, nil
}

func (r *Registry) startStreamingConversion(ctx context.Context, repository, source string) (streamingOperation, error) {
	var operation streamingOperation

	u := url.URL{Scheme: "https", Host: r.host, Path: "/acr/v1/" + repository + "/_streaming/" + source}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewBufferString(`{"conversionFormat":"overlaybd","conversionVersion":"v1"}`))
	if err != nil {
		return operation, err
	}

	req.Header.Set("Content-Type", "application/json")

	response, err := r.client.Do(req)
	if err != nil {
		return operation, err
	}
	defer func() { _ = response.Body.Close() }() //nolint:errcheck // response cleanup

	body, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return operation, err
	}

	if len(body) > 1<<20 {
		return operation, errors.New("ACR conversion response exceeds 1 MiB")
	}

	if response.StatusCode != http.StatusAccepted && response.StatusCode != http.StatusOK {
		return operation, fmt.Errorf("ACR conversion returned HTTP %d: %s", response.StatusCode, body)
	}

	if err := json.Unmarshal(body, &operation); err != nil {
		return operation, fmt.Errorf("decode ACR conversion response: %w", err)
	}

	return operation, nil
}

func (r *Registry) waitStreamingConversion(ctx context.Context, repository string, operation streamingOperation, emit func(string)) error {
	for {
		switch operation.Status {
		case "Succeeded":
			return nil
		case "Failed", "Canceled":
			return fmt.Errorf("ACR conversion %s: %s %s", operation.Status, operation.Details, operation.Error)
		case "NotStarted", "Running":
			if !streamingOperationID.MatchString(operation.ID) {
				return errors.New("ACR conversion returned an invalid operation ID")
			}
		default:
			return fmt.Errorf("ACR conversion returned an unsupported status %q", operation.Status)
		}

		emit("ACR streaming conversion " + operation.Status + " " + operation.Progress)

		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}

		body, err := r.get(ctx, "/acr/v1/"+repository+"/_operations/"+operation.ID, nil)
		if err != nil {
			return fmt.Errorf("read ACR conversion status: %w", err)
		}

		var next streamingOperation
		if err := json.Unmarshal(body, &next); err != nil {
			return fmt.Errorf("decode ACR conversion status: %w", err)
		}

		if next.ID != "" && next.ID != operation.ID {
			return errors.New("ACR conversion operation identity changed")
		}

		// Poll responses omit the ID returned by the create request.
		next.ID = operation.ID
		operation = next
	}
}
