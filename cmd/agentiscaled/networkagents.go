package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The node watches its network's agents on one stream from the control
// service, signed with its own key, the way a Tailscale node holds its map
// poll, and serves them to tools on this machine on the local agent socket
// (GET /network/agents). Tools never hold an owner credential: the node's
// membership is what lets it read the list. The stream is held only while
// a local tool reads, and a little after, so tools asking one after
// another share it.

// Stream timings: how long the stream stays open after the last local
// reader leaves, and how long the node waits before opening it again after
// it could not, or after the control service refused this node.
const (
	networkStreamLinger       = 30 * time.Second
	networkStreamRetry        = 5 * time.Second
	networkStreamRefusedRetry = time.Minute
)

// networkStreamSilence is how long the stream may go without a line, the
// control service's keepalives included, before the node takes it as lost.
const networkStreamSilence = 90 * time.Second

// networkFirstAnswerTimeout bounds how long a one-off read waits for the
// stream's first answer.
const networkFirstAnswerTimeout = 15 * time.Second

// networkAgents is what GET /network/agents answers: the agents of the
// node's network as the owner console shows them, and this node's peer ID,
// so a caller can tell the agents of this machine from the others'.
type networkAgents struct {
	NetworkID         string         `json:"network_id"`
	PeerID            string         `json:"peer_id"`
	ObservedAt        time.Time      `json:"observed_at"`
	StaleAfterSeconds int            `json:"stale_after_seconds"`
	Agents            []networkAgent `json:"agents"`
}

type networkAgent struct {
	PeerID string `json:"peer_id"`
	// MachineName is the name the agent's node gave when it joined.
	MachineName string    `json:"machine_name,omitempty"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ReportedAt  time.Time `json:"reported_at"`
	// ReportStatus is fresh while the agent's node reports to the network,
	// and stale once its last report is older than StaleAfterSeconds: the
	// agent is then unavailable.
	ReportStatus string `json:"report_status"`
	// Activity is set only while the report is fresh.
	Activity *agentActivity `json:"activity,omitempty"`
}

// available says whether the agent's node reports to the network now.
func (a networkAgent) available() bool { return a.ReportStatus == "fresh" }

// errNetworkAgents is an answer the control service gave that says why it
// will not list the agents; its message is safe to show.
type errNetworkAgents struct {
	status  int
	message string
}

func (e *errNetworkAgents) Error() string { return e.message }

// networkState is what the node knows of its network's agents: the latest
// list, or why it has none. version counts changes; zero is nothing yet.
type networkState struct {
	answer  *networkAgents
	err     error
	version uint64
}

// networkAgentsSource holds the node's stream of its network's agents
// while local tools read it.
type networkAgentsSource struct {
	control string
	network string
	priv    crypto.PrivKey
	client  *http.Client // nil: networkStreamClient
	// timings overrides the stream timings; tests shorten them.
	timings *streamTimings

	mu      sync.Mutex
	state   networkState
	changed chan struct{} // closed and replaced on each change
	readers int
	stop    context.CancelFunc // ends the stream; nil when none is held
	linger  *time.Timer
}

type streamTimings struct{ linger, retry, refusedRetry time.Duration }

func (s *networkAgentsSource) timing() streamTimings {
	if s.timings != nil {
		return *s.timings
	}
	return streamTimings{networkStreamLinger, networkStreamRetry, networkStreamRefusedRetry}
}

// networkStreamClient has no overall timeout, since the stream lasts while
// tools read, but bounds the wait for the control service's first answer.
var networkStreamClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 15 * time.Second
	return &http.Client{Transport: transport}
}()

// subscribe registers a local reader, opening the stream if none is held,
// and returns the function that unregisters it.
func (s *networkAgentsSource) subscribe() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers++
	if s.linger != nil {
		s.linger.Stop()
		s.linger = nil
	}
	if s.stop == nil {
		ctx, cancel := context.WithCancel(context.Background())
		s.stop = cancel
		go s.follow(ctx)
	}
	var once sync.Once
	return func() { once.Do(s.unsubscribe) }
}

func (s *networkAgentsSource) unsubscribe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.readers--
	if s.readers > 0 || s.stop == nil {
		return
	}
	s.linger = time.AfterFunc(s.timing().linger, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.readers > 0 || s.stop == nil {
			return
		}
		s.stop()
		s.stop, s.linger = nil, nil
		// A later reader waits for a fresh answer, never this stale one.
		s.state = networkState{}
		s.notifyLocked()
	})
}

// current is the state now, and a channel closed when it changes.
func (s *networkAgentsSource) current() (networkState, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.changed == nil {
		s.changed = make(chan struct{})
	}
	return s.state, s.changed
}

func (s *networkAgentsSource) set(answer *networkAgents, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = networkState{answer: answer, err: err, version: s.state.version + 1}
	s.notifyLocked()
}

func (s *networkAgentsSource) notifyLocked() {
	if s.changed != nil {
		close(s.changed)
	}
	s.changed = make(chan struct{})
}

// get is the list for a one-off read: the stream's latest answer, waiting
// for its first one if the stream just opened.
func (s *networkAgentsSource) get(ctx context.Context) (networkAgents, error) {
	done := s.subscribe()
	defer done()
	ctx, cancel := context.WithTimeout(ctx, networkFirstAnswerTimeout)
	defer cancel()
	for {
		state, changed := s.current()
		if state.version > 0 {
			if state.err != nil {
				return networkAgents{}, state.err
			}
			return *state.answer, nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return networkAgents{}, &errNetworkAgents{http.StatusServiceUnavailable, "the control service did not answer in time"}
		}
	}
}

// follow holds the stream until ctx ends, opening it again when it ends.
func (s *networkAgentsSource) follow(ctx context.Context) {
	for ctx.Err() == nil {
		delivered, err := s.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		var known *errNetworkAgents
		refused := errors.As(err, &known) && (known.status == http.StatusGone || known.status == http.StatusNotFound)
		wait := s.timing().retry
		switch {
		case refused:
			wait = s.timing().refusedRetry
			s.set(nil, err)
		case delivered:
			// A stream that worked and ended, as when the control service
			// restarts, is opened again soon; the list stays meanwhile.
			wait = time.Second
		default:
			s.set(nil, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// stream opens one stream and publishes each line until it ends. It says
// whether it delivered a list, and why it ended.
func (s *networkAgentsSource) stream(ctx context.Context) (bool, error) {
	if s.control == "" || s.network == "" {
		return false, &errNetworkAgents{http.StatusServiceUnavailable, "this node does not know its control service or network; run 'agentiscaled up' again"}
	}
	pid, err := peer.IDFromPrivateKey(s.priv)
	if err != nil {
		return false, err
	}
	ts := time.Now().UnixMilli()
	sig, err := s.priv.Sign(nodeAgentsPayload(s.network, pid.String(), ts))
	if err != nil {
		return false, err
	}
	body, _ := json.Marshal(map[string]any{"network_id": s.network, "peer_id": pid.String(), "timestamp_ms": ts, "signature": base64.RawURLEncoding.EncodeToString(sig)})
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.control, "/")+"/v1/node-agents", bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.client
	if client == nil {
		client = networkStreamClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, &errNetworkAgents{http.StatusServiceUnavailable, "the control service could not be reached"}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, streamRefusal(resp)
	}
	// The control service sends a line, a keepalive at least, every thirty
	// seconds; a stream silent for longer is lost.
	var silent atomic.Bool
	silence := time.AfterFunc(networkStreamSilence, func() {
		silent.Store(true)
		cancel()
	})
	defer silence.Stop()
	lines := bufio.NewReaderSize(resp.Body, 64<<10)
	delivered := false
	for {
		line, err := readLine(lines, 4<<20)
		if err != nil {
			if silent.Load() {
				return delivered, &errNetworkAgents{http.StatusServiceUnavailable, "the control service stopped answering"}
			}
			return delivered, &errNetworkAgents{http.StatusServiceUnavailable, "the control service could not be reached"}
		}
		silence.Reset(networkStreamSilence)
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var failure struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if json.Unmarshal(line, &failure) == nil && failure.Error != "" {
			if failure.Code == "node_removed" {
				return delivered, &errNetworkAgents{http.StatusGone, "this node was removed from the network"}
			}
			s.set(nil, &errNetworkAgents{http.StatusBadGateway, failure.Error})
			continue
		}
		var answer networkAgents
		if err := json.Unmarshal(line, &answer); err != nil {
			return delivered, &errNetworkAgents{http.StatusBadGateway, "the control service answered with an unreadable agent list"}
		}
		if answer.NetworkID != s.network {
			return delivered, &errNetworkAgents{http.StatusBadGateway, "the control service answered for another network"}
		}
		if answer.Agents == nil {
			answer.Agents = []networkAgent{}
		}
		answer.PeerID = pid.String()
		delivered = true
		s.set(&answer, nil)
	}
}

// streamRefusal is why the control service did not open the stream.
func streamRefusal(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusGone:
		return &errNetworkAgents{http.StatusGone, "this node was removed from the network"}
	case http.StatusNotFound:
		// An older control service without the route also answers 404, so
		// only its own code says the network does not know the node.
		var answer struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
		if answer.Code == "unknown_network" {
			return &errNetworkAgents{http.StatusNotFound, "the network does not know this node"}
		}
		return &errNetworkAgents{http.StatusBadGateway, "the control service does not list network agents yet"}
	case http.StatusServiceUnavailable:
		return &errNetworkAgents{http.StatusServiceUnavailable, "the control service is busy; trying again"}
	}
	return &errNetworkAgents{http.StatusBadGateway, fmt.Sprintf("the control service could not list the network's agents (%s)", http.StatusText(resp.StatusCode))}
}

// readLine reads one line of at most max bytes.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > max {
			return nil, errors.New("line too long")
		}
		if !isPrefix {
			return line, nil
		}
	}
}

// nodeAgentsPayload is what the node signs to read its network's agents.
func nodeAgentsPayload(network, peerID string, unixMs int64) []byte {
	return fmt.Appendf(nil, "agentiscale-node-agents\n%s\n%s\n%d", network, peerID, unixMs)
}

// serveNetworkAgents answers GET /network/agents on the local agent socket:
// the list, or with ?watch=1 a stream of it, one JSON object per line: the
// list now, then the list again each time it changes, or {"error": "..."}
// while it can't be read. The stream lasts until the caller leaves.
func serveNetworkAgents(w http.ResponseWriter, r *http.Request, source *networkAgentsSource) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("watch") != "" {
		watchNetworkAgents(w, r, source)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	answer, err := source.get(r.Context())
	if err != nil {
		status, message := networkAgentsFailure(err)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	_ = json.NewEncoder(w).Encode(answer)
}

func networkAgentsFailure(err error) (int, string) {
	var known *errNetworkAgents
	if errors.As(err, &known) {
		return known.status, known.message
	}
	return http.StatusServiceUnavailable, "the network's agents could not be read"
}

func watchNetworkAgents(w http.ResponseWriter, r *http.Request, source *networkAgentsSource) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	done := source.subscribe()
	defer done()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	var last []byte
	for {
		state, changed := source.current()
		if state.version > 0 {
			var line any = map[string]string{"error": ""}
			var answer networkAgents
			if state.err != nil {
				_, message := networkAgentsFailure(state.err)
				line = map[string]string{"error": message}
			} else {
				answer = *state.answer
				line = answer
			}
			// observed_at changes with every list; only a change in what the
			// list says is sent.
			key, _ := json.Marshal(watchKey(answer, state.err))
			if !bytes.Equal(key, last) {
				last = key
				b, _ := json.Marshal(line)
				if _, err := w.Write(append(b, '\n')); err != nil {
					return
				}
				flusher.Flush()
			}
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		}
	}
}

func watchKey(answer networkAgents, err error) any {
	if err != nil {
		_, message := networkAgentsFailure(err)
		return message
	}
	return []any{answer.NetworkID, answer.PeerID, answer.Agents}
}
