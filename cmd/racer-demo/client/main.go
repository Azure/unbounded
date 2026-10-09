// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"io"
	"os"

	"github.com/Azure/unbounded/pkg/racersdk"
)

const cacheName = "racer-demo"

func main() {
	var err error
	if len(os.Args) > 2 && os.Args[1] != "bench" {
		err = runBench()
	} else {
		err = runGet()
	}

	if err != nil {
		panic(err)
	}
}

func runGet() error {
	client, err := racersdk.NewClient(racersdk.ClientConfig{Cache: cacheName})
	if err != nil {
		return err
	}

	defer func() { _ = client.Close() }() //nolint:errcheck

	obj, err := client.Get(context.TODO(), racersdk.Request{
		Key: sha256.Sum256([]byte("0")),
	})
	if err != nil {
		return err
	}

	defer func() { _ = obj.Close() }() //nolint:errcheck

	_, err = io.Copy(os.Stdout, obj)

	return err
}
