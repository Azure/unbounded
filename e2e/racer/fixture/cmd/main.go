// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/Azure/unbounded/e2e/racer/fixture"
	racermeta "github.com/Azure/unbounded/internal/racer"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "serve" {
		origin := fixture.NewOrigin()
		origin.Source = os.Getenv("NODE_NAME")

		for _, name := range os.Args[2:] {
			_, path, err := racermeta.CacheSockets(racermeta.SocketRoot, name)
			if err != nil {
				log.Fatal(err)
			}

			go func() {
				for {
					if err := racermeta.PrepareSocketDirectory(path); err != nil {
						log.Fatal(err)
					}

					listener, err := net.Listen("unix", path)
					if errors.Is(err, syscall.ENOENT) {
						time.Sleep(100 * time.Millisecond)
						continue
					}

					if errors.Is(err, syscall.EADDRINUSE) {
						conn, probeErr := net.DialTimeout("unix", path, time.Second)
						if conn != nil {
							if err := conn.Close(); err != nil {
								log.Fatal(err)
							}
						}

						if errors.Is(probeErr, syscall.ECONNREFUSED) {
							info, statErr := os.Lstat(path)
							if statErr == nil && info.Mode()&os.ModeSocket != 0 {
								if err := os.Remove(path); err != nil {
									log.Fatal(err)
								}

								continue
							}
						}
					}

					if err != nil {
						log.Fatal(err)
					}

					if err := os.Chmod(path, 0o660); err != nil {
						log.Fatal(err)
					}

					log.Fatal((&http.Server{Handler: origin, ReadHeaderTimeout: 5 * time.Second}).Serve(listener))
				}
			}()
		}

		s := &http.Server{Addr: ":8080", Handler: origin, ReadHeaderTimeout: 5 * time.Second}
		log.Fatal(s.ListenAndServe())
	}

	if len(os.Args) < 5 || os.Args[1] != "request" {
		log.Fatal("usage: fixture serve [CACHE_NAME...] | fixture request METHOD URL RANGE [HEADER...]")
	}

	r, err := fixture.Fetch(os.Args[2], os.Args[3], os.Args[4], os.Args[5:]...)
	if err != nil {
		log.Fatal(err)
	}

	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		log.Fatal(err)
	}
}
