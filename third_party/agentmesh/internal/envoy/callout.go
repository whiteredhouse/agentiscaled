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

package envoy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extprocv3http "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extprocv3 "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/google/agentmesh/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/structpb"
)

const (
	// DefaultMessageTimeout is the default per-phase gRPC message timeout for ext_proc callouts.
	DefaultMessageTimeout = 200 * time.Millisecond
	// DefaultMaxBufferBytes is the default body buffer cap for ext_proc callouts (1 MiB).
	DefaultMaxBufferBytes = 1 << 20
)

// CalloutClient manages a google.golang.org/grpc client connection to an
// external Envoy ExternalProcessor service over unix domain sockets, h2c, or TLS/mTLS.
type CalloutClient struct {
	conn   *grpc.ClientConn
	client extprocv3.ExternalProcessorClient
}

// NewCalloutClient constructs a CalloutClient for cfg using google.golang.org/grpc.
func NewCalloutClient(cfg *api.ExtProc, secretsDir string) (*CalloutClient, error) {
	rawTarget := strings.TrimSpace(cfg.GetTarget())
	if rawTarget == "" {
		return nil, errors.New("ext_proc.target is required")
	}

	opts := []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(MaxGRPCMessageBytes),
			grpc.MaxCallSendMsgSize(MaxGRPCMessageBytes),
		),
	}

	if sockPath, ok := strings.CutPrefix(rawTarget, "unix:"); ok {
		sockPath = strings.TrimPrefix(sockPath, "//")
		opts = append(opts,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sockPath)
			}),
		)
		conn, err := grpc.NewClient("passthrough:///unix", opts...)
		if err != nil {
			return nil, err
		}
		return &CalloutClient{conn: conn, client: extprocv3.NewExternalProcessorClient(conn)}, nil
	}

	useTLS := strings.HasPrefix(rawTarget, "https://") || cfg.GetCa() != "" || cfg.GetClientCertificate() != ""
	endpointHost := strings.TrimPrefix(strings.TrimPrefix(rawTarget, "https://"), "http://")
	endpointHost = strings.TrimRight(endpointHost, "/")

	if useTLS {
		tlsCfg := &tls.Config{
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"h2"},
		}
		if caFile := strings.TrimSpace(cfg.GetCa()); caFile != "" {
			if filepath.Base(caFile) != caFile || caFile == "." || caFile == ".." {
				return nil, fmt.Errorf("ext_proc.ca %q must be a file name", caFile)
			}
			pemBytes, err := os.ReadFile(filepath.Join(secretsDir, caFile))
			if err != nil {
				return nil, fmt.Errorf("ext_proc.ca %q: %w", caFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pemBytes) {
				return nil, fmt.Errorf("ext_proc.ca %q: failed to parse PEM certificates", caFile)
			}
			tlsCfg.RootCAs = pool
		}
		if certFile := strings.TrimSpace(cfg.GetClientCertificate()); certFile != "" {
			if filepath.Base(certFile) != certFile || certFile == "." || certFile == ".." {
				return nil, fmt.Errorf("ext_proc.client_certificate %q must be a file name", certFile)
			}
			pemBytes, err := os.ReadFile(filepath.Join(secretsDir, certFile))
			if err != nil {
				return nil, fmt.Errorf("ext_proc.client_certificate %q: %w", certFile, err)
			}
			cert, err := tls.X509KeyPair(pemBytes, pemBytes)
			if err != nil {
				return nil, fmt.Errorf("ext_proc.client_certificate %q: %w", certFile, err)
			}
			tlsCfg.Certificates = []tls.Certificate{cert}
		}
		opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	} else {
		opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	conn, err := grpc.NewClient("passthrough:///"+endpointHost, opts...)
	if err != nil {
		return nil, err
	}
	return &CalloutClient{conn: conn, client: extprocv3.NewExternalProcessorClient(conn)}, nil
}

// Close closes the underlying gRPC ClientConn.
func (c *CalloutClient) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// CalloutSession represents an active bidirectional ExternalProcessor.Process
// stream spanning request and optional response inspection phases.
type CalloutSession struct {
	cfg        *api.ExtProc
	stream     extprocv3.ExternalProcessor_ProcessClient
	cancel     context.CancelFunc
	mode       *extprocv3http.ProcessingMode
	msgTimeout time.Duration
}

// OpenStream opens a new bidirectional Process stream with a stream-level
// deadline derived from timeout.
func (c *CalloutClient) OpenStream(ctx context.Context, cfg *api.ExtProc, timeout time.Duration) (*CalloutSession, error) {
	msgTimeout := DefaultMessageTimeout
	if cfg != nil && cfg.GetMessageTimeout().IsValid() && cfg.GetMessageTimeout().AsDuration() > 0 {
		msgTimeout = cfg.GetMessageTimeout().AsDuration()
	}
	if timeout <= 0 {
		timeout = max(msgTimeout*4, 5*time.Second)
	}
	streamCtx, cancel := context.WithTimeout(ctx, timeout)
	stream, err := c.client.Process(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	return &CalloutSession{
		cfg:        cfg,
		stream:     stream,
		cancel:     cancel,
		mode:       InitialProcessingMode(cfg),
		msgTimeout: msgTimeout,
	}, nil
}

// Send transmits a ProcessingRequest on the stream.
func (s *CalloutSession) Send(req *extprocv3.ProcessingRequest) error {
	return s.stream.Send(req)
}

// Recv waits for the next ProcessingResponse up to timeout (or the session's
// configured message_timeout if timeout <= 0).
func (s *CalloutSession) Recv(timeout time.Duration) (*extprocv3.ProcessingResponse, error) {
	if timeout <= 0 {
		timeout = s.msgTimeout
	}
	if timeout <= 0 {
		timeout = DefaultMessageTimeout
	}
	type recvResult struct {
		resp *extprocv3.ProcessingResponse
		err  error
	}
	ch := make(chan recvResult, 1)
	go func() {
		resp, err := s.stream.Recv()
		ch <- recvResult{resp: resp, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-ch:
		return res.resp, res.err
	case <-timer.C:
		s.Close()
		return nil, fmt.Errorf("ext_proc message_timeout (%s) exceeded", timeout)
	}
}

// CloseSend half-closes the client-to-server direction of the stream.
func (s *CalloutSession) CloseSend() error {
	return s.stream.CloseSend()
}

// Close cancels and tears down the bidirectional stream.
func (s *CalloutSession) Close() {
	if s == nil {
		return
	}
	_ = s.stream.CloseSend()
	s.cancel()
}

// NeedsResponseBuffer reports whether the session's current ProcessingMode
// requires buffering and inspecting upstream response headers or body.
func (s *CalloutSession) NeedsResponseBuffer() bool {
	if s == nil || s.mode == nil {
		return false
	}
	return s.mode.GetResponseHeaderMode() != extprocv3http.ProcessingMode_SKIP ||
		s.mode.GetResponseBodyMode() != extprocv3http.ProcessingMode_NONE
}

// RunRequestPhase executes the request headers and optional request body
// phases on a new CalloutSession.
func (c *CalloutClient) RunRequestPhase(
	r *http.Request,
	cfg *api.ExtProc,
	destName string,
	reqBody []byte,
	attrs map[string]*structpb.Struct,
) (*CalloutSession, *extprocv3.ImmediateResponse, []byte, error) {
	msgTimeout := DefaultMessageTimeout
	if cfg.GetMessageTimeout().IsValid() && cfg.GetMessageTimeout().AsDuration() > 0 {
		msgTimeout = cfg.GetMessageTimeout().AsDuration()
	}
	session, err := c.OpenStream(r.Context(), cfg, max(msgTimeout*4, 5*time.Second))
	if err != nil {
		return nil, nil, reqBody, err
	}

	if session.mode.GetRequestHeaderMode() != extprocv3http.ProcessingMode_SKIP {
		endOfStream := len(reqBody) == 0 || session.mode.GetRequestBodyMode() == extprocv3http.ProcessingMode_NONE
		err := session.Send(&extprocv3.ProcessingRequest{
			Attributes: attrs,
			Request: &extprocv3.ProcessingRequest_RequestHeaders{
				RequestHeaders: &extprocv3.HttpHeaders{
					Headers:     HTTPRequestToProtoHeaders(r, destName),
					EndOfStream: endOfStream,
				},
			},
		})
		if err != nil {
			session.Close()
			return nil, nil, reqBody, err
		}
		resp, err := session.Recv(session.msgTimeout)
		if err != nil {
			session.Close()
			return nil, nil, reqBody, err
		}
		if cfg.GetAllowModeOverride() && resp.GetModeOverride() != nil {
			ApplyModeOverride(session.mode, resp.GetModeOverride())
		}
		if resp.GetOverrideMessageTimeout().IsValid() && resp.GetOverrideMessageTimeout().AsDuration() > 0 {
			session.msgTimeout = resp.GetOverrideMessageTimeout().AsDuration()
		}
		if imm := resp.GetImmediateResponse(); imm != nil {
			return session, imm, reqBody, nil
		}
		if hr := resp.GetRequestHeaders().GetResponse(); hr != nil {
			ApplySafeHeaderMutations(r.Header, hr.GetHeaderMutation())
			if bm := hr.GetBodyMutation(); bm != nil {
				if bm.GetClearBody() {
					reqBody = nil
				} else if bm.GetBody() != nil {
					reqBody = bm.GetBody()
				}
			}
		}
	}

	if len(reqBody) > 0 && session.mode.GetRequestBodyMode() != extprocv3http.ProcessingMode_NONE {
		err := session.Send(&extprocv3.ProcessingRequest{
			Attributes: attrs,
			Request: &extprocv3.ProcessingRequest_RequestBody{
				RequestBody: &extprocv3.HttpBody{
					Body:        reqBody,
					EndOfStream: true,
				},
			},
		})
		if err != nil {
			session.Close()
			return nil, nil, reqBody, err
		}
		resp, err := session.Recv(session.msgTimeout)
		if err != nil {
			session.Close()
			return nil, nil, reqBody, err
		}
		if cfg.GetAllowModeOverride() && resp.GetModeOverride() != nil {
			ApplyModeOverride(session.mode, resp.GetModeOverride())
		}
		if imm := resp.GetImmediateResponse(); imm != nil {
			return session, imm, reqBody, nil
		}
		if br := resp.GetRequestBody().GetResponse(); br != nil {
			ApplySafeHeaderMutations(r.Header, br.GetHeaderMutation())
			if bm := br.GetBodyMutation(); bm != nil {
				if bm.GetClearBody() {
					reqBody = nil
				} else if bm.GetBody() != nil {
					reqBody = bm.GetBody()
				}
			}
		}
	}

	return session, nil, reqBody, nil
}

// RunResponsePhase executes the response headers and optional response body
// phases on an active CalloutSession.
func (s *CalloutSession) RunResponsePhase(
	status int,
	respHeader http.Header,
	respBody []byte,
) (*extprocv3.ImmediateResponse, []byte, error) {
	defer func() { _ = s.CloseSend() }()

	if s.mode.GetResponseHeaderMode() != extprocv3http.ProcessingMode_SKIP {
		endOfStream := len(respBody) == 0 || s.mode.GetResponseBodyMode() == extprocv3http.ProcessingMode_NONE
		err := s.Send(&extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseHeaders{
				ResponseHeaders: &extprocv3.HttpHeaders{
					Headers:     HTTPResponseToProtoHeaders(status, respHeader),
					EndOfStream: endOfStream,
				},
			},
		})
		if err != nil {
			return nil, respBody, err
		}
		resp, err := s.Recv(s.msgTimeout)
		if err != nil {
			return nil, respBody, err
		}
		if s.cfg.GetAllowModeOverride() && resp.GetModeOverride() != nil {
			ApplyModeOverride(s.mode, resp.GetModeOverride())
		}
		if imm := resp.GetImmediateResponse(); imm != nil {
			return imm, respBody, nil
		}
		if hr := resp.GetResponseHeaders().GetResponse(); hr != nil {
			ApplySafeHeaderMutations(respHeader, hr.GetHeaderMutation())
			if bm := hr.GetBodyMutation(); bm != nil {
				if bm.GetClearBody() {
					respBody = nil
				} else if bm.GetBody() != nil {
					respBody = bm.GetBody()
				}
			}
		}
	}

	if len(respBody) > 0 && s.mode.GetResponseBodyMode() != extprocv3http.ProcessingMode_NONE {
		maxBytes := int(s.cfg.GetMaxBufferedBytes())
		if maxBytes <= 0 {
			maxBytes = DefaultMaxBufferBytes
		}
		if len(respBody) > maxBytes {
			return nil, respBody, fmt.Errorf("response body (%d bytes) exceeds max_buffered_bytes (%d)", len(respBody), maxBytes)
		}
		err := s.Send(&extprocv3.ProcessingRequest{
			Request: &extprocv3.ProcessingRequest_ResponseBody{
				ResponseBody: &extprocv3.HttpBody{
					Body:        respBody,
					EndOfStream: true,
				},
			},
		})
		if err != nil {
			return nil, respBody, err
		}
		resp, err := s.Recv(s.msgTimeout)
		if err != nil {
			return nil, respBody, err
		}
		if imm := resp.GetImmediateResponse(); imm != nil {
			return imm, respBody, nil
		}
		if br := resp.GetResponseBody().GetResponse(); br != nil {
			ApplySafeHeaderMutations(respHeader, br.GetHeaderMutation())
			if bm := br.GetBodyMutation(); bm != nil {
				if bm.GetClearBody() {
					respBody = nil
				} else if bm.GetBody() != nil {
					respBody = bm.GetBody()
				}
			}
		}
	}
	return nil, respBody, nil
}

// IsProtectedEgressHeader reports whether a header name is off-limits to an
// ext_proc inspector. Inspectors may add/modify application headers, or block a
// request, but must never select or overwrite credentials, host routing, or
// Agent Mesh identity headers.
func IsProtectedEgressHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch lower {
	case "authorization", "host", ":authority", "cookie":
		return true
	}
	return strings.HasPrefix(lower, "x-mesh-") || strings.HasPrefix(lower, "x-forwarded-")
}

// ApplySafeHeaderMutations applies non-protected header mutations from mut to h.
func ApplySafeHeaderMutations(h http.Header, mut *extprocv3.HeaderMutation) {
	if mut == nil {
		return
	}
	for _, rem := range mut.GetRemoveHeaders() {
		if IsProtectedEgressHeader(rem) {
			continue
		}
		h.Del(rem)
	}
	for _, opt := range mut.GetSetHeaders() {
		hv := opt.GetHeader()
		if hv == nil {
			continue
		}
		key := strings.TrimSpace(hv.GetKey())
		if key == "" || strings.HasPrefix(key, ":") || IsProtectedEgressHeader(key) {
			continue
		}
		val := hv.GetValue()
		if val == "" && len(hv.GetRawValue()) > 0 {
			val = string(hv.GetRawValue())
		}
		switch opt.GetAppendAction() {
		case corev3.HeaderValueOption_ADD_IF_ABSENT:
			if h.Get(key) == "" {
				h.Set(key, val)
			}
		case corev3.HeaderValueOption_OVERWRITE_IF_EXISTS:
			if h.Get(key) != "" {
				h.Set(key, val)
			}
		default:
			h.Set(key, val)
		}
	}
}

// InitialProcessingMode converts an api.ExtProc configuration into Envoy's
// extprocv3http.ProcessingMode.
func InitialProcessingMode(cfg *api.ExtProc) *extprocv3http.ProcessingMode {
	mode := &extprocv3http.ProcessingMode{
		RequestHeaderMode:   extprocv3http.ProcessingMode_SEND,
		ResponseHeaderMode:  extprocv3http.ProcessingMode_SEND,
		RequestBodyMode:     extprocv3http.ProcessingMode_NONE,
		ResponseBodyMode:    extprocv3http.ProcessingMode_NONE,
		RequestTrailerMode:  extprocv3http.ProcessingMode_SKIP,
		ResponseTrailerMode: extprocv3http.ProcessingMode_SKIP,
	}
	if cfg == nil || cfg.GetProcessingMode() == nil {
		return mode
	}
	pm := cfg.GetProcessingMode()
	if pm.GetRequestHeaderMode() == api.ExtProcProcessingMode_SKIP {
		mode.RequestHeaderMode = extprocv3http.ProcessingMode_SKIP
	}
	if pm.GetResponseHeaderMode() == api.ExtProcProcessingMode_SKIP {
		mode.ResponseHeaderMode = extprocv3http.ProcessingMode_SKIP
	}
	mode.RequestBodyMode = extprocv3http.ProcessingMode_BodySendMode(pm.GetRequestBodyMode())
	mode.ResponseBodyMode = extprocv3http.ProcessingMode_BodySendMode(pm.GetResponseBodyMode())
	if pm.GetRequestTrailerMode() == api.ExtProcProcessingMode_SEND {
		mode.RequestTrailerMode = extprocv3http.ProcessingMode_SEND
	}
	if pm.GetResponseTrailerMode() == api.ExtProcProcessingMode_SEND {
		mode.ResponseTrailerMode = extprocv3http.ProcessingMode_SEND
	}
	return mode
}

// ApplyModeOverride merges a callout server's ModeOverride into dst.
func ApplyModeOverride(dst, override *extprocv3http.ProcessingMode) {
	if dst == nil || override == nil {
		return
	}
	if override.GetRequestHeaderMode() != extprocv3http.ProcessingMode_DEFAULT {
		dst.RequestHeaderMode = override.GetRequestHeaderMode()
	}
	if override.GetResponseHeaderMode() != extprocv3http.ProcessingMode_DEFAULT {
		dst.ResponseHeaderMode = override.GetResponseHeaderMode()
	}
	if override.GetRequestBodyMode() != extprocv3http.ProcessingMode_NONE {
		dst.RequestBodyMode = override.GetRequestBodyMode()
	}
	if override.GetResponseBodyMode() != extprocv3http.ProcessingMode_NONE {
		dst.ResponseBodyMode = override.GetResponseBodyMode()
	}
	if override.GetRequestTrailerMode() != extprocv3http.ProcessingMode_DEFAULT {
		dst.RequestTrailerMode = override.GetRequestTrailerMode()
	}
	if override.GetResponseTrailerMode() != extprocv3http.ProcessingMode_DEFAULT {
		dst.ResponseTrailerMode = override.GetResponseTrailerMode()
	}
}

// HTTPRequestToProtoHeaders converts an outbound HTTP request into an Envoy
// HeaderMap with protected headers stripped.
func HTTPRequestToProtoHeaders(r *http.Request, destName string) *corev3.HeaderMap {
	var list []*corev3.HeaderValue
	if r != nil {
		list = append(list,
			&corev3.HeaderValue{Key: ":method", Value: r.Method},
			&corev3.HeaderValue{Key: ":path", Value: r.URL.RequestURI()},
			&corev3.HeaderValue{Key: ":authority", Value: destName},
			&corev3.HeaderValue{Key: ":scheme", Value: "https"},
		)
		for k, vals := range r.Header {
			if IsProtectedEgressHeader(k) {
				continue
			}
			list = append(list, &corev3.HeaderValue{
				Key:   strings.ToLower(k),
				Value: strings.Join(vals, ", "),
			})
		}
	}
	return &corev3.HeaderMap{Headers: list}
}

// HTTPResponseToProtoHeaders converts an HTTP response status and header map
// into an Envoy HeaderMap.
func HTTPResponseToProtoHeaders(status int, h http.Header) *corev3.HeaderMap {
	list := []*corev3.HeaderValue{
		{Key: ":status", Value: strconv.Itoa(status)},
	}
	for k, vals := range h {
		list = append(list, &corev3.HeaderValue{
			Key:   strings.ToLower(k),
			Value: strings.Join(vals, ", "),
		})
	}
	return &corev3.HeaderMap{Headers: list}
}
