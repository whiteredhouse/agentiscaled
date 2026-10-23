package node

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/agentmesh/api"
	"github.com/libp2p/go-libp2p/core/peer"
)

// localA2AProviders includes hosted agents without querying or dialing self.
// Other service types keep their existing remote discovery semantics.
func (n *AgentMeshNode) localA2AProviders(kind api.ServiceType, name string) []*api.DiscoveredProvider {
	if kind != api.ServiceType_SERVICE_TYPE_A2A {
		return nil
	}
	var out []*api.DiscoveredProvider
	for _, svc := range n.ListLocalServices(kind) {
		if name != "" && svc.Name != name {
			continue
		}
		out = append(out, &api.DiscoveredProvider{PeerId: n.Host.ID().String(), SrvName: svc.Name, SrvDescription: svc.Description, LocalProxyUrl: n.localProxyURL(n.Host.ID(), api.ServiceTypeStringA2A, svc.Name)})
	}
	return out
}

// localA2ATransport sends self-directed A2A traffic through the ordinary
// ingress handler. Egress still performs its provider/label checks first.
// A pipe keeps responses streaming and cancellation reaches the backend.
type localA2ATransport struct {
	node   *AgentMeshNode
	remote http.RoundTripper
}

func (t *localA2ATransport) RoundTrip(r *http.Request) (*http.Response, error) {
	pid, err := peer.Decode(r.URL.Host)
	if r.URL.Scheme != "libp2p" || err != nil || pid != t.node.Host.ID() || !strings.HasPrefix(r.URL.Path, "/a2a/") {
		return t.remote.RoundTrip(r)
	}
	ctx, cancel := context.WithCancel(r.Context())
	req := r.Clone(ctx)
	req.RemoteAddr = t.node.Host.ID().String()
	req.RequestURI = req.URL.RequestURI()
	if req.Body == nil {
		req.Body = http.NoBody
	}
	reader, writer := io.Pipe()
	ready := make(chan *http.Response, 1)
	w := &localResponseWriter{header: make(http.Header), body: writer, ready: ready, request: r}
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()); _ = writer.CloseWithError(ctx.Err()) })
	go func() {
		defer stop()
		defer writer.Close()
		// net/http normally recovers handler panics. The in-process path must
		// provide that boundary too, especially ReverseProxy's ErrAbortHandler.
		defer func() {
			if p := recover(); p != nil {
				if p != http.ErrAbortHandler {
					logger.Errorf("[Local A2A] ingress panic: %v", p)
				}
				_ = writer.CloseWithError(fmt.Errorf("local ingress aborted"))
				w.finishHeader(http.StatusInternalServerError)
			}
		}()
		if req.Body != nil {
			defer req.Body.Close()
		}
		t.node.ingressHandler().ServeHTTP(w, req)
		w.finishHeader(http.StatusOK)
	}()
	select {
	case resp := <-ready:
		resp.Body = &localResponseBody{ReadCloser: reader, cancel: cancel}
		return resp, nil
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
}

type localResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *localResponseBody) Close() error { b.cancel(); return b.ReadCloser.Close() }

type localResponseWriter struct {
	header  http.Header
	body    *io.PipeWriter
	ready   chan *http.Response
	request *http.Request
	sent    bool
}

func (w *localResponseWriter) Header() http.Header { return w.header }
func (w *localResponseWriter) finishHeader(code int) {
	if w.sent {
		return
	}
	w.sent = true
	w.ready <- &http.Response{StatusCode: code, Status: fmt.Sprintf("%d %s", code, http.StatusText(code)), Header: w.header.Clone(), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, ContentLength: -1, Request: w.request}
}
func (w *localResponseWriter) WriteHeader(code int) {
	if code >= 200 {
		w.finishHeader(code)
	}
}
func (w *localResponseWriter) Write(b []byte) (int, error) {
	w.finishHeader(http.StatusOK)
	return w.body.Write(b)
}
func (w *localResponseWriter) Flush() { w.finishHeader(http.StatusOK) }
