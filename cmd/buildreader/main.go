/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command buildreader is the entrypoint of the story 8.7f reader-pod image (ISI-2905 / S4a of
// ISI-3956). It serves the in-pod, READ-ONLY list/read HTTP protocol jailed at the ReadOnly-mounted
// Project PVC (/workspace) and nothing else — no exec, no write, no delete. The apiserver (S4b)
// reaches it over an in-cluster ClusterIP.
//
// It carries ZERO Kubernetes-client dependencies on purpose: the reader image is the epic's largest
// trust boundary, so it stays as small as possible.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readserver"
)

func main() {
	root := envOr("KSQUAD_WORKSPACE_ROOT", readserver.DefaultRoot)
	addr := envOr("KSQUAD_READER_ADDR", ":8080")

	srv, err := readserver.New(root)
	if err != nil {
		log.Fatalf("buildreader: jail %q: %v", root, err)
	}

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGTERM so an idle-teardown / ActiveDeadline reap is clean.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("buildreader: serving RO list/read on %s jailed at %s", addr, root)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("buildreader: serve: %v", err)
		}
	}()

	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
