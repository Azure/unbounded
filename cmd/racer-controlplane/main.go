// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/Azure/unbounded/internal/racer"
	controlplane "github.com/Azure/unbounded/internal/racer-controlplane"
	"github.com/Azure/unbounded/internal/version"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Println(version.String())
		return
	}

	showVersion := flag.Bool("version", false, "print version and exit")
	listen := flag.String("listen", ":8443", "mTLS control listen address")
	enrollListen := flag.String("enroll-listen", ":8444", "server-authenticated HTTPS enrollment listen address")
	namespace := flag.String("state-namespace", "racer-system", "namespace for durable state and leader election")
	probes := flag.String("health-listen", ":8081", "health probe listen address")
	bootstrap := flag.String("bootstrap-node", "", "print shell bootstrap identities for this Kubernetes Node and exit")
	bootstrapUniverse := flag.String("bootstrap-universe", "", "required mapped Site universe from the Pod universe label")
	bootstrapService := flag.String("bootstrap-service", "racer-controlplane", "controller Service name for bootstrap")
	bootstrapNamespace := flag.String("bootstrap-namespace", "racer-system", "controller Service namespace for bootstrap")
	socketRoot := flag.String("socket-root", racer.SocketRoot, "deployment-wide absolute directory for local cache and origin sockets")
	bootstrapPort := flag.String("bootstrap-port", "8443", "controller Service port name or number")
	reviewQPS := flag.Float64("token-review-qps", 20, "TokenReview API requests per second on credential cache misses")
	reviewBurst := flag.Int("token-review-burst", 30, "TokenReview API request burst on credential cache misses")
	rotationInterval := flag.Duration("ca-rotation-interval", 30*24*time.Hour, "time between CA rotations")
	logging := zap.Options{}
	logging.BindFlags(flag.CommandLine)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&logging)))

	if *bootstrap != "" {
		if err := controlplane.PrintBootstrap(*bootstrap, *bootstrapUniverse, *bootstrapNamespace, *bootstrapService, *bootstrapPort); err != nil {
			log.Fatal(err)
		}

		return
	}

	if _, _, err := racer.CacheSockets(*socketRoot, strings.Repeat("a", 63)); err != nil {
		log.Fatal(err)
	}

	if err := controlplane.Run(*listen, *enrollListen, *namespace, *probes, *socketRoot, *reviewQPS, *reviewBurst, *rotationInterval); err != nil {
		log.Fatal(err)
	}
}
