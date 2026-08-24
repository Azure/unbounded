// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package gh talks to the GitHub REST API on behalf of relctl.
//
// Auth deliberately prefers an existing `gh` login over a provisioned token.
// Every maintainer who can cut a release already has `gh` working, and asking
// them to mint a PAT to run a read-only `relctl status` would be the kind of
// friction that stops a tool being used. In a workflow, GITHUB_TOKEN is set and
// `gh` may not be installed at all, so the environment is checked first.
package gh

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/google/go-github/v75/github"
)

// DefaultRepo is the repository relctl operates on unless told otherwise.
const DefaultRepo = "Azure/unbounded"

// Client is a GitHub API client scoped to one repository.
type Client struct {
	api   *github.Client
	owner string
	repo  string
}

// Repo returns the owner/name slug this client is scoped to.
func (c *Client) Repo() string { return c.owner + "/" + c.repo }

// Owner returns the repository owner.
func (c *Client) Owner() string { return c.owner }

// Name returns the repository name.
func (c *Client) Name() string { return c.repo }

// API exposes the underlying client for calls this package does not wrap.
func (c *Client) API() *github.Client { return c.api }

// SplitRepo parses an owner/name slug.
func SplitRepo(slug string) (owner, name string, err error) {
	owner, name, found := strings.Cut(slug, "/")
	if !found || owner == "" || name == "" {
		return "", "", fmt.Errorf("repository must be OWNER/NAME, got %q", slug)
	}

	if strings.Contains(name, "/") {
		return "", "", fmt.Errorf("repository must be OWNER/NAME, got %q", slug)
	}

	return owner, name, nil
}

// ErrNoToken is returned when no credential could be found by any route.
var ErrNoToken = errors.New(
	"no GitHub credential found: set GITHUB_TOKEN, or run 'gh auth login'")

// TokenSource resolves a GitHub credential. Replaced in tests.
type TokenSource func(ctx context.Context) (string, error)

// EnvOrGH takes GITHUB_TOKEN or GH_TOKEN when set, and otherwise borrows the
// token `gh` already holds.
//
// The environment wins because that is the workflow case, where `gh` may be
// absent and a token is always present. Shelling out is the interactive case.
func EnvOrGH(ctx context.Context) (string, error) {
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if token := strings.TrimSpace(os.Getenv(key)); token != "" {
			return token, nil
		}
	}

	path, err := exec.LookPath("gh")
	if err != nil {
		return "", ErrNoToken
	}

	// Stderr is discarded: `gh auth token` writes its own advice there when not
	// logged in, and ErrNoToken says the same thing with both routes named.
	out, err := exec.CommandContext(ctx, path, "auth", "token").Output()
	if err != nil {
		return "", ErrNoToken
	}

	token := strings.TrimSpace(string(out))
	if token == "" {
		return "", ErrNoToken
	}

	return token, nil
}

// Options configure a Client.
type Options struct {
	// Repo is the owner/name slug. Empty means DefaultRepo.
	Repo string
	// Token supplies the credential. Nil means EnvOrGH.
	Token TokenSource
	// BaseURL points the client at another API root, for tests.
	BaseURL string
}

// New builds a Client.
//
// Constructed lazily by the commands that need it: version resolution is pure
// git and must keep working with no credential at all, including inside a
// workflow that has not been granted one.
func New(ctx context.Context, opts Options) (*Client, error) {
	slug := opts.Repo
	if slug == "" {
		slug = DefaultRepo
	}

	owner, name, err := SplitRepo(slug)
	if err != nil {
		return nil, err
	}

	source := opts.Token
	if source == nil {
		source = EnvOrGH
	}

	token, err := source(ctx)
	if err != nil {
		return nil, err
	}

	api := github.NewClient(nil).WithAuthToken(token)

	if opts.BaseURL != "" {
		api, err = api.WithEnterpriseURLs(opts.BaseURL, opts.BaseURL)
		if err != nil {
			return nil, fmt.Errorf("set base URL: %w", err)
		}
	}

	return &Client{api: api, owner: owner, repo: name}, nil
}
