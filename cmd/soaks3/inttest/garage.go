// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/wait"
)

// garageImage is the Garage S3 store image. Pinned to match the tag the
// orca integration suite uses (internal/orca/inttest); bump in lockstep.
const garageImage = "dxflrs/garage:v1.0.1"

// garageRegion is the S3 region Garage signs against. The signed client
// the harness uses for uploads must use the same region.
const garageRegion = "us-east-1"

// webHost is the host name soaks3 addresses the web endpoint with. Garage
// serves a bucket anonymously over its web endpoint when the request Host
// (port stripped) matches one of the bucket's global aliases, so the
// harness aliases the test bucket to this name. "localhost" is the only
// reliably valid alias (Garage rejects bare IPs like 127.0.0.1), and the
// published container port is reachable on localhost on Linux runners.
const webHost = "localhost"

// garageConfig is the garage.toml mounted into the container. Single
// node, sqlite metadata, replication factor 1, with both the S3 API
// (signed, used for uploads) and the web endpoint (anonymous, used by
// soaks3's unsigned GETs) enabled. The rpc_secret and admin_token are
// throwaway constants for an ephemeral container, not credentials.
const garageConfig = `
metadata_dir = "/tmp/garage/meta"
data_dir = "/tmp/garage/data"
db_engine = "sqlite"
replication_factor = 1
rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

[s3_api]
s3_region = "us-east-1"
api_bind_addr = "[::]:3900"
root_domain = ".s3.garage"

[s3_web]
bind_addr = "[::]:3902"
root_domain = ".web.garage"
index = "index.html"

[admin]
api_bind_addr = "[::]:3903"
admin_token = "soaks3-dev-admin-token"
`

var (
	keyIDRe     = regexp.MustCompile(`(?m)^Key ID:\s+(\S+)`)
	keySecretRe = regexp.MustCompile(`(?m)^Secret key:\s+(\S+)`)
)

// Garage is a single-node Garage container exposing both a signed S3 API
// (for uploading the seed data set) and an anonymous web endpoint (for
// soaks3's unsigned read load).
type Garage struct {
	container   testcontainers.Container
	s3Endpoint  string
	webEndpoint string
	accessKey   string
	secretKey   string
}

// WebEndpoint returns the http:// URL of the anonymous web endpoint that
// soaks3 reads from. soaks3 should run with an empty --bucket so the
// generated URLs are path-style against this endpoint.
func (g *Garage) WebEndpoint() string { return g.webEndpoint }

// StartGarage launches a single-node Garage container, bootstraps a
// usable cluster with an S3 key, and returns a handle once the S3 API is
// reachable. Caller terminates the container (e.g. via t.Cleanup).
func StartGarage(ctx context.Context) (*Garage, error) {
	req := testcontainers.ContainerRequest{
		Image:        garageImage,
		ExposedPorts: []string{"3900/tcp", "3902/tcp"},
		Files: []testcontainers.ContainerFile{
			{
				Reader:            strings.NewReader(garageConfig),
				ContainerFilePath: "/etc/garage.toml",
				FileMode:          0o644,
			},
		},
		WaitingFor: wait.ForListeningPort("3900/tcp").WithStartupTimeout(60 * time.Second),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start garage: %w", err)
	}

	g := &Garage{container: c}

	if err := g.bootstrap(ctx); err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, err
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("garage host: %w", err)
	}

	s3Port, err := c.MappedPort(ctx, "3900/tcp")
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("garage s3 port: %w", err)
	}

	webPort, err := c.MappedPort(ctx, "3902/tcp")
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("garage web port: %w", err)
	}

	g.s3Endpoint = fmt.Sprintf("http://%s:%s", host, s3Port.Port())
	// soaks3 must reach the web endpoint with Host=webHost so Garage's
	// alias match works, so address it by webHost rather than c.Host.
	g.webEndpoint = fmt.Sprintf("http://%s:%s", webHost, webPort.Port())

	return g, nil
}

// bootstrap turns a freshly-started node into a usable single-node
// cluster with an S3 key: wait for the node, assign + apply a layout,
// create a key with create-bucket rights, and capture its credentials.
func (g *Garage) bootstrap(ctx context.Context) error {
	nodeID, err := g.pollNodeID(ctx)
	if err != nil {
		return err
	}

	steps := [][]string{
		{"/garage", "-c", "/etc/garage.toml", "layout", "assign", nodeID, "-z", "dc1", "-c", "1G"},
		{"/garage", "-c", "/etc/garage.toml", "layout", "apply", "--version", "1"},
		{"/garage", "-c", "/etc/garage.toml", "key", "create", "soaks3-dev"},
		{"/garage", "-c", "/etc/garage.toml", "key", "allow", "--create-bucket", "soaks3-dev"},
	}
	for _, step := range steps {
		if out, err := g.exec(ctx, step); err != nil {
			return fmt.Errorf("garage bootstrap %q: %w (output: %s)", strings.Join(step, " "), err, out)
		}
	}

	info, err := g.exec(ctx, []string{"/garage", "-c", "/etc/garage.toml", "key", "info", "--show-secret", "soaks3-dev"})
	if err != nil {
		return fmt.Errorf("garage key info: %w (output: %s)", err, info)
	}

	idMatch := keyIDRe.FindStringSubmatch(info)
	secretMatch := keySecretRe.FindStringSubmatch(info)

	if idMatch == nil || secretMatch == nil {
		return fmt.Errorf("garage key info: could not parse credentials from output: %s", info)
	}

	g.accessKey = idMatch[1]
	g.secretKey = secretMatch[1]

	return nil
}

// pollNodeID waits for the node to register and returns its hex id.
func (g *Garage) pollNodeID(ctx context.Context) (string, error) {
	deadline := time.Now().Add(30 * time.Second)

	for {
		out, err := g.exec(ctx, []string{"/garage", "-c", "/etc/garage.toml", "node", "id", "-q"})
		if err == nil {
			id := strings.TrimSpace(out)
			if at := strings.IndexByte(id, '@'); at > 0 {
				id = id[:at]
			}

			if id != "" {
				return id, nil
			}
		}

		if time.Now().After(deadline) {
			return "", fmt.Errorf("garage node did not become ready within 30s (last: %s)", out)
		}

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// exec runs a command in the container and returns its combined output.
func (g *Garage) exec(ctx context.Context, cmd []string) (string, error) {
	code, reader, err := g.container.Exec(ctx, cmd, tcexec.Multiplexed())
	if err != nil {
		return "", err
	}

	var out string

	if reader != nil {
		b, _ := io.ReadAll(reader) //nolint:errcheck // best-effort capture
		out = string(b)
	}

	if code != 0 {
		return out, fmt.Errorf("exit code %d", code)
	}

	return out, nil
}

// Terminate stops and removes the Garage container.
func (g *Garage) Terminate(ctx context.Context) error {
	return g.container.Terminate(ctx)
}

// s3Client returns a signed AWS S3 client pointed at the Garage S3 API
// with path-style addressing and the bootstrapped credentials. It is
// used to create the bucket and upload the seed data set.
func (g *Garage) s3Client(ctx context.Context, t *testing.T) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(
		ctx,
		awsconfig.WithRegion(garageRegion),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			g.accessKey, g.secretKey, "",
		)),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(g.s3Endpoint)
		o.UsePathStyle = true
	})
}

// PrepareBucket creates a bucket, grants the bootstrapped key read/write
// access (for uploads), enables anonymous web access on it, and aliases
// it to webHost so soaks3's unsigned path-style GETs against the web
// endpoint resolve to it. Returns the bucket name for uploads. The bucket
// is created via the garage CLI rather than the S3 API so it gets a clean
// global alias the subsequent CLI steps can address. No t.Cleanup hook is
// registered; the whole container is torn down instead.
func (g *Garage) PrepareBucket(ctx context.Context, t *testing.T) string {
	t.Helper()

	const bucket = "soaks3-bucket"

	steps := [][]string{
		{"/garage", "-c", "/etc/garage.toml", "bucket", "create", bucket},
		{"/garage", "-c", "/etc/garage.toml", "bucket", "allow", "--read", "--write", "--owner", bucket, "--key", "soaks3-dev"},
		// Serve the bucket anonymously over the web endpoint and alias it
		// to webHost so a request with Host=webHost selects it unsigned.
		{"/garage", "-c", "/etc/garage.toml", "bucket", "website", "--allow", bucket},
		{"/garage", "-c", "/etc/garage.toml", "bucket", "alias", bucket, webHost},
	}
	for _, step := range steps {
		if out, err := g.exec(ctx, step); err != nil {
			t.Fatalf("garage %q: %v (output: %s)", strings.Join(step, " "), err, out)
		}
	}

	return bucket
}
