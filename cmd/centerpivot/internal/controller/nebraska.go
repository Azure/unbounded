// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/Azure/unbounded/cmd/centerpivot/internal/omaha"
)

// Nebraska sends update checks. ca holds the PEM certificates that sign
// Nebraska's; with none, the system's are used.
type Nebraska interface {
	UpdateCheck(ctx context.Context, ca string, check omaha.Check) (omaha.Result, error)
}

// nebraskaTimeout bounds one update check.
const nebraskaTimeout = 30 * time.Second

// HTTPNebraska sends update checks over HTTPS, with one client per set of
// CA certificates.
type HTTPNebraska struct {
	mu      sync.Mutex
	clients map[string]*omaha.Client
}

// UpdateCheck implements Nebraska.
func (n *HTTPNebraska) UpdateCheck(ctx context.Context, ca string, check omaha.Check) (omaha.Result, error) {
	client, err := n.client(ca)
	if err != nil {
		return omaha.Result{}, err
	}

	return client.UpdateCheck(ctx, check)
}

func (n *HTTPNebraska) client(ca string) (*omaha.Client, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if client, ok := n.clients[ca]; ok {
		return client, nil
	}

	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("the default HTTP transport isn't an *http.Transport")
	}

	transport = transport.Clone()

	if ca != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("the CA ConfigMap's ca.crt holds no PEM certificate")
		}

		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}

	if n.clients == nil {
		n.clients = map[string]*omaha.Client{}
	}

	client := &omaha.Client{HTTP: &http.Client{Transport: transport, Timeout: nebraskaTimeout}}
	n.clients[ca] = client

	return client, nil
}
