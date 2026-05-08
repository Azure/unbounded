// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integrationtest

package inttest

import (
	"context"
	"fmt"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// LocalStack is a running LocalStack container with helper accessors
// for constructing AWS S3 clients pointed at it. Use NewS3Client to
// get a configured client; use NewBucket to allocate a fresh bucket
// for a single test.
type LocalStack struct {
	container testcontainers.Container
	endpoint  string
	region    string
}

// AccessKey returns the LocalStack-default access key. LocalStack does
// not validate credentials but the AWS SDK requires non-empty values.
func (ls *LocalStack) AccessKey() string { return "test" }

// SecretKey returns the LocalStack-default secret key.
func (ls *LocalStack) SecretKey() string { return "test" }

// Endpoint returns the http:// URL of the LocalStack edge port.
func (ls *LocalStack) Endpoint() string { return ls.endpoint }

// Region returns the static region the harness uses with LocalStack.
func (ls *LocalStack) Region() string { return ls.region }

// StartLocalStack launches a LocalStack container and returns a handle
// once the edge port is healthy. Caller is responsible for terminating
// the container (via container.Terminate or t.Cleanup).
func StartLocalStack(ctx context.Context) (*LocalStack, error) {
	req := testcontainers.ContainerRequest{
		Image:        localstackImage,
		ExposedPorts: []string{"4566/tcp"},
		Env: map[string]string{
			"SERVICES": "s3",
			// LocalStack 3.8 returns InvalidRequest on the SDK's
			// CRC64NVME default checksum. The orca s3 driver opts out
			// at the SDK config level, but seeding clients in tests
			// must do the same. We set the variables both in the
			// container env (for any in-container tooling) and on the
			// SDK config in NewS3Client.
			"S3_SKIP_SIGNATURE_VALIDATION": "1",
		},
		WaitingFor: wait.ForHTTP("/_localstack/health").
			WithPort("4566/tcp").
			WithStatusCodeMatcher(func(status int) bool { return status == 200 }),
	}

	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("start localstack: %w", err)
	}

	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("localstack host: %w", err)
	}

	port, err := c.MappedPort(ctx, "4566/tcp")
	if err != nil {
		_ = c.Terminate(ctx) //nolint:errcheck // best-effort cleanup
		return nil, fmt.Errorf("localstack port: %w", err)
	}

	return &LocalStack{
		container: c,
		endpoint:  fmt.Sprintf("http://%s:%s", host, port.Port()),
		region:    "us-east-1",
	}, nil
}

// Terminate stops and removes the LocalStack container.
func (ls *LocalStack) Terminate(ctx context.Context) error {
	return ls.container.Terminate(ctx)
}

// NewS3Client returns an AWS S3 client with LocalStack-friendly
// settings (path-style addressing, dummy credentials, checksum quirks
// disabled).
func (ls *LocalStack) NewS3Client(ctx context.Context, t *testing.T) *s3.Client {
	t.Helper()

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(ls.region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			ls.AccessKey(), ls.SecretKey(), "",
		)),
		awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired),
		awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}

	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ls.endpoint)
		o.UsePathStyle = true
	})
}

// NewBucket creates a fresh bucket and registers a t.Cleanup hook to
// best-effort delete it. Returns the bucket name.
func (ls *LocalStack) NewBucket(ctx context.Context, t *testing.T, prefix string) string {
	t.Helper()

	cli := ls.NewS3Client(ctx, t)
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

// EnableVersioning toggles versioning on a bucket. Used by the
// versioning-gate negative test.
func (ls *LocalStack) EnableVersioning(ctx context.Context, t *testing.T, bucket string) {
	t.Helper()

	cli := ls.NewS3Client(ctx, t)
	if _, err := cli.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket: aws.String(bucket),
		VersioningConfiguration: &s3types.VersioningConfiguration{
			Status: s3types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatalf("enable versioning on %s: %v", bucket, err)
	}
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
