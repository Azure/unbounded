// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type originS3Client struct {
	*s3.Client
	pool originConnectionPool
}

func (c *originS3Client) Close() error {
	c.pool.close()
	return nil
}

func newOriginS3Client(cfg aws.Config, o options) (*originS3Client, error) {
	owned := &originS3Client{}

	client := s3.NewFromConfig(cfg, func(s *s3.Options) {
		s.UsePathStyle = o.pathStyle
		if o.endpoint != "" {
			s.BaseEndpoint = aws.String(o.endpoint)
		}

		if buildable, ok := s.HTTPClient.(*awshttp.BuildableClient); ok {
			s.HTTPClient = buildable.WithTransportOptions(func(tr *http.Transport) {
				tr.DialContext = owned.pool.dial(tr.DialContext)
			})
		}
	})
	if err := validateEndpoint(aws.ToString(client.Options().BaseEndpoint)); err != nil {
		return nil, safeDiagnostic("invalid effective S3 endpoint: must be an HTTP(S) origin without credentials, query, fragment, or path prefix, with a valid port", "config", err)
	}

	owned.Client = client

	return owned, nil
}
