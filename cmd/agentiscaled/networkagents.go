package main

import (
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
	"time"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// The node reads its network's agents from the control service, signed
// with its own key, and serves them to tools on this machine on the local
// agent socket (GET /network/agents). Tools never hold an owner credential:
// the node's membership is what lets it read the list.

// networkAgentsTTL is how long one answer from the control service serves
// local callers, so several tools polling the socket ask the service once.
const networkAgentsTTL = 5 * time.Second

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
	PeerID      string    `json:"peer_id"`
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

// networkAgentsSource asks the control service for the node's network's
// agents and keeps the last answer for networkAgentsTTL.
type networkAgentsSource struct {
	control string
	network string
	priv    crypto.PrivKey
	client  *http.Client
	now     func() time.Time

	mu     sync.Mutex
	last   *networkAgents
	readAt time.Time
}

func (s *networkAgentsSource) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *networkAgentsSource) get(ctx context.Context) (networkAgents, error) {
	// One question at a time: callers that arrive while it is asked get its
	// answer, and the service sees one request per interval.
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last != nil && s.clock().Sub(s.readAt) < networkAgentsTTL {
		return *s.last, nil
	}
	answer, err := s.ask(ctx)
	if err != nil {
		return networkAgents{}, err
	}
	s.last, s.readAt = &answer, s.clock()
	return answer, nil
}

func (s *networkAgentsSource) ask(ctx context.Context) (networkAgents, error) {
	if s.control == "" || s.network == "" {
		return networkAgents{}, &errNetworkAgents{http.StatusServiceUnavailable, "this node does not know its control service or network; run 'agentiscaled up' again"}
	}
	pid, err := peer.IDFromPrivateKey(s.priv)
	if err != nil {
		return networkAgents{}, err
	}
	ts := time.Now().UnixMilli()
	sig, err := s.priv.Sign(nodeAgentsPayload(s.network, pid.String(), ts))
	if err != nil {
		return networkAgents{}, err
	}
	body, _ := json.Marshal(map[string]any{"network_id": s.network, "peer_id": pid.String(), "timestamp_ms": ts, "signature": base64.RawURLEncoding.EncodeToString(sig)})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(s.control, "/")+"/v1/node-agents", bytes.NewReader(body))
	if err != nil {
		return networkAgents{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.client
	if client == nil {
		client = httpClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return networkAgents{}, &errNetworkAgents{http.StatusServiceUnavailable, "the control service could not be reached"}
	}
	defer resp.Body.Close()
	limited := io.LimitReader(resp.Body, 4<<20)
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusGone:
		return networkAgents{}, &errNetworkAgents{http.StatusGone, "this node was removed from the network"}
	case http.StatusNotFound:
		// An older control service without the route also answers 404, so
		// only its own code says the network does not know the node.
		var answer struct {
			Code string `json:"code"`
		}
		_ = json.NewDecoder(limited).Decode(&answer)
		if answer.Code == "unknown_network" {
			return networkAgents{}, &errNetworkAgents{http.StatusNotFound, "the network does not know this node"}
		}
		return networkAgents{}, &errNetworkAgents{http.StatusBadGateway, "the control service does not list network agents yet"}
	default:
		return networkAgents{}, &errNetworkAgents{http.StatusBadGateway, fmt.Sprintf("the control service could not list the network's agents (%s)", http.StatusText(resp.StatusCode))}
	}
	var answer networkAgents
	if err := json.NewDecoder(limited).Decode(&answer); err != nil {
		return networkAgents{}, &errNetworkAgents{http.StatusBadGateway, "the control service answered with an unreadable agent list"}
	}
	if answer.NetworkID != s.network {
		return networkAgents{}, &errNetworkAgents{http.StatusBadGateway, "the control service answered for another network"}
	}
	if answer.Agents == nil {
		answer.Agents = []networkAgent{}
	}
	answer.PeerID = pid.String()
	return answer, nil
}

// nodeAgentsPayload is what the node signs to read its network's agents.
func nodeAgentsPayload(network, peerID string, unixMs int64) []byte {
	return fmt.Appendf(nil, "agentiscale-node-agents\n%s\n%s\n%d", network, peerID, unixMs)
}

// serveNetworkAgents answers GET /network/agents on the local agent socket.
func serveNetworkAgents(w http.ResponseWriter, r *http.Request, source *networkAgentsSource) {
	answer, err := source.get(r.Context())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		status, message := http.StatusServiceUnavailable, "the network's agents could not be read"
		var known *errNetworkAgents
		if errors.As(err, &known) {
			status, message = known.status, known.message
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
		return
	}
	_ = json.NewEncoder(w).Encode(answer)
}
