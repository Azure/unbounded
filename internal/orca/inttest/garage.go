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

// garageRegion is the S3 region Garage is configured with (see
// garageConfig). Garage validates the SigV4 region against this value,
// so the S3 clients the harness builds must sign with the same region.
const garageRegion = "us-east-1"

// garageConfig is the garage.toml mounted into the container. Single
// node, sqlite metadata engine, replication factor 1. The rpc_secret
// and admin_token are throwaway constants for an ephemeral test
// container, not credentials.
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

[admin]
api_bind_addr = "[::]:3903"
admin_token = "orca-dev-admin-token"
`

// Garage is a running single-node Garage container with helper
// accessors for constructing AWS S3 clients pointed at it. It replaces
// the previous LocalStack harness: Orca's cachestore commit is
// stat-then-put (HeadObject then PutObject) and needs no conditional
// write, so any S3-compatible store works. Use NewS3Client to get a
// configured client; use NewBucket to allocate a fresh bucket for a
// single test.
type Garage struct {
	container testcontainers.Container
	endpoint  string
	accessKey string
	secretKey string
}

// AccessKey returns the access key bootstrapped for the test key.
func (g *Garage) AccessKey() string { return g.accessKey }

// SecretKey returns the secret key bootstrapped for the test key.
func (g *Garage) SecretKey() string { return g.secretKey }

// Endpoint returns the http:// URL of the Garage S3 API port.
func (g *Garage) Endpoint() string { return g.endpoint }

// Region returns the static region the harness uses with Garage.
func (g *Garage) Region() string { return garageRegion }

// keyInfoRe extracts the access key id and secret from
// `garage key info --show-secret` output.
var (
	keyIDRe     = regexp.MustCompile(`(?m)^Key ID:\s+(\S+)`)
	keySecretRe = regexp.MustCompile(`(?m)^Secret key:\s+(\S+)`)
)

// StartGarage launches a single-node Garage container, assigns and
// applies a cluster layout, creates an S3 key with bucket-creation
// rights, and returns a handle once the S3 API is reachable. Caller is
// responsible for terminating the container (via container.Terminate or
// t.Cleanup).
func StartGarage(ctx context.Context) (*Garage, error) {
	req := testcontainers.ContainerRequest{
		Image:        garageImage,
		ExposedPorts: []string{"3900/tcp"},
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

	port, err := c.MappedPort(ctx, "3900/tcp")
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("garage port: %w", err)
	}

	g.endpoint = fmt.Sprintf("http://%s:%s", host, port.Port())

	return g, nil
}

// bootstrap runs the in-container garage CLI sequence that turns a
// freshly-started node into a usable single-node cluster with an S3 key:
// wait for the node to appear, assign + apply a layout, create a key
// with create-bucket rights, and capture its credentials.
func (g *Garage) bootstrap(ctx context.Context) error {
	// Resolve the node id (output is "<hex>@<addr>"; layout assign
	// accepts the hex id or a unique prefix).
	nodeID, err := g.pollNodeID(ctx)
	if err != nil {
		return err
	}

	steps := [][]string{
		{"/garage", "-c", "/etc/garage.toml", "layout", "assign", nodeID, "-z", "dc1", "-c", "1G"},
		{"/garage", "-c", "/etc/garage.toml", "layout", "apply", "--version", "1"},
		{"/garage", "-c", "/etc/garage.toml", "key", "create", "orca-dev"},
		{"/garage", "-c", "/etc/garage.toml", "key", "allow", "--create-bucket", "orca-dev"},
	}
	for _, step := range steps {
		if out, err := g.exec(ctx, step); err != nil {
			return fmt.Errorf("garage bootstrap %q: %w (output: %s)", strings.Join(step, " "), err, out)
		}
	}

	// Capture the generated credentials.
	info, err := g.exec(ctx, []string{"/garage", "-c", "/etc/garage.toml", "key", "info", "--show-secret", "orca-dev"})
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

// pollNodeID waits for the node to register and returns its id (the hex
// portion before the @ in `garage node id`).
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
// A non-zero exit code is returned as an error.
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

// NewS3Client returns an AWS S3 client with Garage-friendly settings
// (path-style addressing, the bootstrapped credentials, checksum quirks
// disabled to match the cachestore/s3 driver).
func (g *Garage) NewS3Client(ctx context.Context, t *testing.T) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(g.Region()),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			g.AccessKey(), g.SecretKey(), "",
		)),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(g.endpoint)
		o.UsePathStyle = true
	})
}

// NewBucket creates a fresh bucket and registers a t.Cleanup hook to
// best-effort delete it. Returns the bucket name.
func (g *Garage) NewBucket(ctx context.Context, t *testing.T, prefix string) string {
	t.Helper()

	cli := g.NewS3Client(ctx, t)
	name := uniqueName(prefix)

	if _, err := cli.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(name),
	}); err != nil {
		t.Fatalf("create bucket %s: %v", name, err)
	}

	t.Cleanup(func() {
		emptyBucket(context.Background(), cli, name)

		_, _ = cli.DeleteBucket(context.Background(), &s3.DeleteBucketInput{ //nolint:errcheck // best-effort cleanup
			Bucket: aws.String(name),
		})
	})

	return name
}

// emptyBucket deletes every object in the bucket. Best-effort; errors
// are ignored.
func emptyBucket(ctx context.Context, cli *s3.Client, bucket string) {
	out, err := cli.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		return
	}

	for _, obj := range out.Contents {
		_, _ = cli.DeleteObject(ctx, &s3.DeleteObjectInput{ //nolint:errcheck // best-effort cleanup
			Bucket: aws.String(bucket),
			Key:    obj.Key,
		})
	}
}
