// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package gantryui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/distribution/reference"
	godigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/Azure/unbounded/internal/gantry/manifest"
	"github.com/Azure/unbounded/internal/gantry/progress"
)

type Layer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type Image struct {
	ID           string  `json:"id"`
	Repository   string  `json:"repository"`
	Tag          string  `json:"tag"`
	Digest       string  `json:"digest"`
	Reference    string  `json:"reference"`
	Architecture string  `json:"architecture"`
	OS           string  `json:"os"`
	Streaming    bool    `json:"streaming"`
	Layers       []Layer `json:"layers"`
	Size         int64   `json:"size"`
}

type Catalog struct {
	Registry string   `json:"registry"`
	Images   []Image  `json:"images"`
	Errors   []string `json:"errors"`
}

type ImageCatalog interface {
	Catalog(context.Context) (Catalog, error)
}

type Registry struct {
	host         string
	architecture string
	client       *auth.Client
}

var (
	acrHost          = regexp.MustCompile(`^[a-z0-9]{5,50}\.azurecr\.io$`)
	registryErrorURL = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

type registryRequestError struct{ err error }

func (failure registryRequestError) Error() string {
	return registryErrorURL.ReplaceAllStringFunc(failure.err.Error(), func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil {
			return "[registry URL redacted]"
		}

		u.RawQuery, u.Fragment, u.User = "", "", nil

		return u.String()
	})
}

func (failure registryRequestError) Unwrap() error { return failure.err }

func NewRegistry(host, architecture string, credential azcore.TokenCredential) (*Registry, error) {
	if !acrHost.MatchString(host) {
		return nil, errors.New("registry must be a public-cloud ACR login server such as example.azurecr.io")
	}

	if architecture != "amd64" && architecture != "arm64" {
		return nil, errors.New("architecture must be amd64 or arm64")
	}

	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyFromEnvironment,
			DialContext:       (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2: true, MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
			IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second,
		},
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// ACR metadata blobs may redirect to signed regional/blob URLs.
			// Never forward registry credentials to those data endpoints.
			req.Header.Del("Authorization")

			target := req.URL.Hostname()
			if len(via) > 3 || req.URL.Scheme != "https" || req.URL.User != nil ||
				(target != host && !strings.HasSuffix(target, ".blob.core.windows.net") &&
					!strings.HasSuffix(target, ".data.azurecr.io")) {
				return errors.New("refusing an unsupported registry metadata redirect")
			}

			return nil
		},
	}
	r := &Registry{host: host, architecture: architecture}
	r.client = &auth.Client{
		Client: client,
		Cache:  auth.NewCache(),
		Credential: func(ctx context.Context, requestedHost string) (auth.Credential, error) {
			if requestedHost != host {
				return auth.EmptyCredential, errors.New("refusing credentials for another registry")
			}

			token, err := credential.GetToken(ctx, policy.TokenRequestOptions{
				Scopes: []string{"https://containerregistry.azure.net/.default"},
			})
			if err != nil {
				return auth.EmptyCredential, fmt.Errorf("authenticate Azure registry identity: %w", err)
			}

			form := url.Values{
				"grant_type":   {"access_token"},
				"service":      {host},
				"access_token": {token.Token},
			}

			req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+host+"/oauth2/exchange", strings.NewReader(form.Encode()))
			if err != nil {
				return auth.EmptyCredential, err
			}

			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

			resp, err := client.Do(req)
			if err != nil {
				return auth.EmptyCredential, fmt.Errorf("ACR token exchange: %w", registryRequestError{err: err})
			}

			defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response cleanup

			if resp.StatusCode != http.StatusOK {
				return auth.EmptyCredential, fmt.Errorf("ACR token exchange returned HTTP %d", resp.StatusCode)
			}

			var result struct {
				RefreshToken string `json:"refresh_token"`
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil {
				return auth.EmptyCredential, err
			}

			if result.RefreshToken == "" {
				return auth.EmptyCredential, errors.New("ACR returned an empty refresh token")
			}

			return auth.Credential{RefreshToken: result.RefreshToken}, nil
		},
	}

	return r, nil
}

func (r *Registry) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	u := url.URL{Scheme: "https", Host: r.host, Path: path, RawQuery: query.Encode()}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", strings.Join([]string{
		ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex,
		"application/vnd.docker.distribution.manifest.v2+json",
		"application/vnd.docker.distribution.manifest.list.v2+json",
	}, ", "))

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry request: %w", registryRequestError{err: err})
	}

	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // response cleanup

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry %s returned HTTP %d", path, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil {
		return nil, err
	}

	if len(body) > 4<<20 {
		return nil, errors.New("registry metadata exceeds 4 MiB")
	}

	expected := path[strings.LastIndex(path, "/")+1:]
	if strings.HasPrefix(expected, "sha256:") && godigest.FromBytes(body).String() != expected {
		return nil, errors.New("registry metadata digest mismatch")
	}

	return body, nil
}

func (r *Registry) names(ctx context.Context, path string) ([]string, error) {
	result := []string{}

	last := ""
	for {
		body, err := r.get(ctx, path, url.Values{"n": {"100"}, "last": {last}})
		if err != nil {
			return nil, err
		}

		var page struct {
			Repositories []string `json:"repositories"`
			Tags         []string `json:"tags"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}

		names := append(page.Repositories, page.Tags...)
		if len(names) == 0 {
			return result, nil
		}

		if len(result)+len(names) > 10000 || names[len(names)-1] == last {
			return nil, errors.New("registry pagination exceeded its bound or stopped advancing")
		}

		result = append(result, names...)
		last = names[len(names)-1]
		// Follow the cursor even for short pages: registries may impose a
		// smaller page size than requested.
	}
}

func (r *Registry) Catalog(ctx context.Context) (Catalog, error) {
	catalog := Catalog{Registry: r.host, Images: []Image{}, Errors: []string{}}

	repositories, err := r.names(ctx, "/v2/_catalog")
	if err != nil {
		return catalog, err
	}

	type result struct {
		image *Image
		err   error
	}

	results := make(chan result)
	group := new(errgroup.Group)
	group.SetLimit(8)

	go func() {
		defer func() {
			// Cancellation can stop repository enumeration while earlier
			// image workers are still sending their final results.
			_ = group.Wait() //nolint:errcheck // context error is returned below

			close(results)
		}()

		for _, repository := range repositories {
			if _, err := reference.WithName(r.host + "/" + repository); err != nil {
				results <- result{err: fmt.Errorf("invalid repository %q", repository)}
				continue
			}

			tags, err := r.names(ctx, "/v2/"+repository+"/tags/list")
			if err != nil {
				select {
				case results <- result{err: err}:
				case <-ctx.Done():
					return
				}

				continue
			}

			for _, tag := range tags {
				group.Go(func() error {
					image, err := r.image(ctx, repository, tag)
					select {
					case results <- result{image: image, err: err}:
					case <-ctx.Done():
						return ctx.Err()
					}

					return nil
				})
			}
		}
	}()

	for result := range results {
		if result.err != nil {
			catalog.Errors = append(catalog.Errors, result.err.Error())
		} else if result.image != nil {
			catalog.Images = append(catalog.Images, *result.image)
		}
	}

	if err := ctx.Err(); err != nil {
		return catalog, err
	}

	sort.Slice(catalog.Images, func(i, j int) bool {
		a, b := catalog.Images[i], catalog.Images[j]
		return a.Repository+":"+a.Tag < b.Repository+":"+b.Tag
	})

	return catalog, nil
}

func (r *Registry) image(ctx context.Context, repository, tag string) (*Image, error) {
	body, err := r.get(ctx, "/v2/"+repository+"/manifests/"+tag, nil)
	if err != nil {
		return nil, err
	}

	var envelope struct {
		Manifests []ocispec.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}

	if len(envelope.Manifests) > 0 {
		found := false

		for _, child := range envelope.Manifests {
			if child.Platform != nil && child.Platform.OS == "linux" && child.Platform.Architecture == r.architecture {
				body, err = r.get(ctx, "/v2/"+repository+"/manifests/"+child.Digest.String(), nil)
				if err != nil {
					return nil, err
				}

				found = true

				break
			}
		}

		if !found {
			return nil, nil
		}
	}

	var m ocispec.Manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}

	streaming := manifest.IsOverlayBDStreamingManifest(body)
	if (m.ArtifactType != "" && !streaming) || m.Config.Digest == "" {
		return nil, nil
	}

	if len(m.Layers) > progress.MaxLayers {
		return nil, fmt.Errorf("%s:%s exceeds %d layers", repository, tag, progress.MaxLayers)
	}

	config, err := r.get(ctx, "/v2/"+repository+"/blobs/"+m.Config.Digest.String(), nil)
	if err != nil {
		return nil, err
	}

	var cfg ocispec.Image
	if err := json.Unmarshal(config, &cfg); err != nil {
		return nil, err
	}

	d := godigest.FromBytes(body).String()
	ref := r.host + "/" + repository + "@" + d
	hash := sha256.Sum256([]byte(ref))

	image := &Image{
		ID: hex.EncodeToString(hash[:10]), Repository: repository, Tag: tag,
		Digest: d, Reference: ref, Architecture: cfg.Architecture, OS: cfg.OS,
		Streaming: streaming, Layers: []Layer{},
	}
	for _, layer := range m.Layers {
		if err := layer.Digest.Validate(); err != nil || layer.Digest.Algorithm() != godigest.SHA256 ||
			layer.Size < 0 || layer.Size > math.MaxInt64-image.Size || len(layer.URLs) > 0 {
			return nil, fmt.Errorf("%s:%s has an unsupported layer descriptor", repository, tag)
		}

		image.Layers = append(image.Layers, Layer{Digest: layer.Digest.String(), Size: layer.Size})
		image.Size += layer.Size
	}

	return image, nil
}
