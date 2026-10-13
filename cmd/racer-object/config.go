// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func newOriginS3Client(cfg aws.Config, o options) (*s3.Client, error) {
	client := s3.NewFromConfig(cfg, func(s *s3.Options) {
		s.UsePathStyle = o.pathStyle
		if o.endpoint != "" {
			s.BaseEndpoint = aws.String(o.endpoint)
		}
	})
	if err := validateEndpoint(aws.ToString(client.Options().BaseEndpoint)); err != nil {
		return nil, errors.New("invalid effective S3 endpoint: must be an HTTP(S) origin without credentials, query, fragment, or path prefix, with a valid port")
	}

	return client, nil
}
