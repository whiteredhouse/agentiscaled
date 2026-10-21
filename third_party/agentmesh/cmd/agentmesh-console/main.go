// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/google/agentmesh/internal/console"
	"github.com/google/agentmesh/internal/secrets"
	"github.com/google/agentmesh/internal/version"
)

func main() {
	var (
		controlPlaneURL = flag.String("control-plane", "http://localhost:8080", "URL of the Agent Mesh control plane")
		adminTokenPath  = flag.String("admin-token-path", "", "Path to file containing the admin token (or env AGENTMESH_ADMIN_TOKEN)")
		bindAddr        = flag.String("bind-addr", ":8081", "Address to bind the console server")
		staticDir       = flag.String("static-dir", "", "Directory containing static frontend files (default: assets embedded in the binary)")
		basePath        = flag.String("base-path", "", "Base path prefix for the console (e.g. /console)")
		externalURL     = flag.String("external-url", "", "Origin browsers reach this console on, e.g. https://console.example. Sets the OIDC redirect_uri and cookie Secure flag instead of trusting the Host and X-Forwarded-Proto headers")
		showVersion     = flag.Bool("version", false, "Print the version and exit")
	)
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String())
		return
	}

	adminToken, err := secrets.FromPathOrEnv("admin-token", *adminTokenPath, "AGENTMESH_ADMIN_TOKEN")
	if err != nil {
		log.Fatalf("%v", err)
	}
	if adminToken == "" {
		log.Fatal("Admin token is required (via --admin-token-path or env AGENTMESH_ADMIN_TOKEN)")
	}
	if *externalURL == "" {
		log.Print("WARNING: --external-url is not set; the OIDC redirect_uri and the session cookie's Secure flag follow the Host and X-Forwarded-Proto request headers, which a client controls. Set --external-url when the console is reachable beyond localhost.")
	}

	srv, err := console.NewServer(console.Config{
		ControlPlaneURL: *controlPlaneURL,
		AdminToken:      adminToken,
		StaticDir:       *staticDir,
		StaticFS:        console.EmbeddedAssets(),
		BasePath:        console.NormalizeBasePath(*basePath),
		ExternalURL:     *externalURL,
	})
	if err != nil {
		log.Fatalf("Failed to initialize console server: %v", err)
	}

	httpSrv := &http.Server{
		Addr:    *bindAddr,
		Handler: srv.Handler(),
		// Mitigate Slowloris-style resource exhaustion from slow/malicious clients.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		log.Printf("Agent Mesh Console listening on %s", *bindAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Console server error: %v", err)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	log.Println("Shutting down console server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server shutdown failed: %v", err)
	}
}
