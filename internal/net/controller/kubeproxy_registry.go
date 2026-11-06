// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"

	"github.com/Azure/unbounded/internal/ociutil"
)

const (
	kubeProxyRegistryLookupTimeout = 30 * time.Second
	kubeProxyRegistryMaxPages      = 100
)

// registryKubeProxyTag searches only the chosen repository, never changing the
// registry or image path to find a tag. Listing is anonymous; explicit versions
// and provider tags do not need this lookup.
func registryKubeProxyTag(ctx context.Context, repository, serverTag string) (string, error) {
	repo, err := newKubeProxyTagRepository(repository)
	if err != nil {
		return "", err
	}

	return kubeProxyTagFromRepository(ctx, repo, serverTag)
}

func newKubeProxyTagRepository(repository string) (*remote.Repository, error) {
	repo, err := remote.NewRepository(kubeProxyRegistryReference(repository))
	if err != nil {
		return nil, fmt.Errorf("parse kube-proxy repository %q: %w", repository, err)
	}

	ociutil.ConfigurePlainHTTP(repo)
	repo.TagListPageSize = 1000
	repo.TagListMaxPages = kubeProxyRegistryMaxPages
	repo.MaxMetadataBytes = 32 << 20
	repo.Client = newKubeProxyRegistryClient(retry.DefaultClient)

	return repo, nil
}

// kubeProxyRegistryReference normalizes the implicit Docker Hub registry and
// official-image namespace for lookup only. The managed image keeps the name
// supplied by the user or provider.
func kubeProxyRegistryReference(repository string) string {
	host, path, hasSlash := strings.Cut(repository, "/")
	if !hasSlash {
		return "docker.io/library/" + repository
	}

	if host != "localhost" && !strings.ContainsAny(host, ".:") && strings.ToLower(host) == host {
		return "docker.io/" + repository
	}

	switch strings.ToLower(host) {
	case "docker.io", "index.docker.io", "registry-1.docker.io":
		if !strings.Contains(path, "/") {
			path = "library/" + path
		}

		return "docker.io/" + path
	default:
		return repository
	}
}

func kubeProxyTagFromRepository(ctx context.Context, repo *remote.Repository, serverTag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, kubeProxyRegistryLookupTimeout)
	defer cancel()

	selection := kubeProxyTagSelection{version: strings.TrimPrefix(serverTag, "v")}

	err := repo.Tags(ctx, "", func(tags []string) error {
		for _, tag := range tags {
			selection.consider(tag)
		}

		return nil
	})
	if err != nil {
		return "", fmt.Errorf("list kube-proxy tags in %q: %w", repo.Reference.String(), err)
	}

	if selection.tag == "" {
		return "", fmt.Errorf("no kube-proxy tag in %q matches Kubernetes version %s", repo.Reference.String(), serverTag)
	}

	return selection.tag, nil
}

// kubeProxyTagSelection ranks exact release tags below their numeric -N
// maintenance revisions. Semver comparison would do the opposite, treating -N
// as a prerelease. Matching is exact, so 1.36.2 never selects 1.36.20 or an rc.
type kubeProxyTagSelection struct {
	version     string
	tag         string
	revision    uint64
	maintenance bool
}

func (s *kubeProxyTagSelection) consider(tag string) {
	version := strings.TrimPrefix(tag, "v")

	var (
		revision    uint64
		maintenance bool
	)

	if version != s.version {
		suffix, ok := strings.CutPrefix(version, s.version+"-")
		if !ok {
			return
		}

		parsed, err := strconv.ParseUint(suffix, 10, 64)
		if err != nil {
			return
		}

		revision, maintenance = parsed, true
	}

	if s.tag != "" {
		if maintenance != s.maintenance {
			if !maintenance {
				return
			}
		} else if revision != s.revision {
			if revision < s.revision {
				return
			}
		} else {
			// Prefer the conventional leading v, then break equivalent-tag
			// ties alphabetically so registry page ordering cannot decide.
			prefixed, currentPrefixed := strings.HasPrefix(tag, "v"), strings.HasPrefix(s.tag, "v")
			if prefixed != currentPrefixed {
				if !prefixed {
					return
				}
			} else if tag >= s.tag {
				return
			}
		}
	}

	s.tag, s.revision, s.maintenance = tag, revision, maintenance
}

// kubeProxyRegistryClient uses ORAS for anonymous authentication and tag-list
// pagination. A registry redirect may end at a different host that challenges
// for its own anonymous token; retry there without forwarding origin credentials.
type kubeProxyRegistryClient struct {
	client *auth.Client
}

func newKubeProxyRegistryClient(client *http.Client) *kubeProxyRegistryClient {
	if client == nil {
		client = retry.DefaultClient
	}

	copy := *client
	checkRedirect := client.CheckRedirect
	copy.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 registry redirects")
		}

		if len(via) > 0 && via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing HTTPS-to-HTTP registry redirect")
		}

		if checkRedirect != nil {
			return checkRedirect(req, via)
		}

		return nil
	}

	authClient := &auth.Client{Client: &copy, Cache: auth.NewCache()}
	authClient.SetUserAgent("unbounded-net-controller")

	return &kubeProxyRegistryClient{client: authClient}
}

func (c *kubeProxyRegistryClient) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode != http.StatusUnauthorized || resp.Request == nil || sameKubeProxyRegistryOrigin(req.URL, resp.Request.URL) {
		return resp, nil
	}

	redirected := resp.Request.Clone(req.Context())
	redirected.Header.Del("Authorization")
	resp.Body.Close() //nolint:errcheck // best effort close before the anonymous retry

	return c.client.Do(redirected)
}

func sameKubeProxyRegistryOrigin(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}
