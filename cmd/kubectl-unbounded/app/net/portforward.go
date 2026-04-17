// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package net

import (
	"io"
	"net/http"
	"net/url"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// spdyRoundTripperForConfig builds the SPDY transport used by port-forward.
func spdyRoundTripperForConfig(cfg *rest.Config) (http.RoundTripper, spdy.Upgrader, error) {
	return spdy.RoundTripperFor(cfg)
}

// newPortForwarder creates a new client-go port forwarder.
func newPortForwarder(
	targetURL *url.URL,
	transport http.RoundTripper,
	upgrader spdy.Upgrader,
	ports []string,
	stopCh <-chan struct{},
	readyCh chan struct{},
	outW io.Writer,
	errW io.Writer,
	addresses []string,
) (*portforward.PortForwarder, error) {
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, targetURL)
	return portforward.NewOnAddresses(dialer, addresses, ports, stopCh, readyCh, outW, errW)
}
