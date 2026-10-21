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

// Package pprofserver serves Go's runtime profiles on a listener of their
// own. A profile is how a memory or goroutine figure that stays high after
// a load leaves is attributed to the code holding it; the metrics say that
// it is held, not by what. The listener is unauthenticated and a CPU
// profile occupies the process for its duration, so it is off unless an
// operator names an address, and that address should be loopback or a
// cluster-internal one reached through a port-forward.
package pprofserver

import (
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// Handler serves the pprof index and every profile under /debug/pprof/ on
// a mux of its own. Importing net/http/pprof also registers them on
// http.DefaultServeMux; no mesh component serves that mux.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

// Serve listens on addr and serves Handler until the returned server is
// shut down.
func Serve(addr string) (*http.Server, error) {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{
		Addr:              listener.Addr().String(),
		Handler:           Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	return server, nil
}
