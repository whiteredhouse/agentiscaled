package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	meshnode "github.com/google/agentmesh/export/node"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// A node retries its network for as long as it runs, which is right for an
// outage and wrong when the network is gone for good: deleted, or no longer
// admitting the node because the node was removed, its credential is
// refused or its network's signing key is one it no longer trusts. Then it
// stops, says so in one line in its log and in `agentiscaled status`, and
// waits for `agentiscaled reset`.

// nodeHalt is why a node stopped retrying its network, kept in haltFile so
// that the stop survives a restart of the background service.
type nodeHalt struct {
	Reason  string    `json:"reason"`
	Network string    `json:"network,omitempty"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

const haltFile = "node-halted.json"

// Reasons a node halts.
const (
	haltNetworkGone        = "network_gone"
	haltRemoved            = "removed"
	haltCredentialRefused  = "credential_refused"
	haltSigningKeyRejected = "signing_key_rejected"
)

func loadHalt(dataDir string) (*nodeHalt, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, haltFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var h nodeHalt
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, fmt.Errorf("%s: %w", haltFile, err)
	}
	return &h, nil
}

func saveHalt(dataDir string, h nodeHalt) error {
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dataDir, haltFile), append(b, '\n'), 0o600)
}

// newHalt is a halt with its one line, which names the fix.
func newHalt(reason, network, dataDir string) nodeHalt {
	fix := resetCommand(dataDir)
	name := "network " + network
	if network == "" {
		name = "its network"
	}
	var msg string
	switch reason {
	case haltNetworkGone:
		msg = fmt.Sprintf("This node stopped: %s no longer exists. Run '%s' to join your current network.", name, fix)
	case haltRemoved:
		msg = fmt.Sprintf("This node stopped: it was removed from %s. Run '%s' to join again as a new node.", name, fix)
	case haltCredentialRefused:
		msg = fmt.Sprintf("This node stopped: %s no longer accepts its credential. Run '%s' to join again.", name, fix)
	case haltSigningKeyRejected:
		msg = fmt.Sprintf("This node stopped: %s is signed with a key this node no longer trusts. Run '%s' to join again.", name, fix)
	}
	return nodeHalt{Reason: reason, Network: network, Message: msg, At: time.Now().UTC()}
}

func resetCommand(dataDir string) string {
	if dataDir == defaultDataDir() {
		return "agentiscaled reset"
	}
	return "agentiscaled reset -data-dir " + shellQuote(dataDir)
}

// meshAnswer is what a failed pull from the network's mesh says about the
// node's standing in it.
type meshAnswer int

const (
	meshFine meshAnswer = iota
	// meshUnreachable is an outage: no answer, a timeout, a 5xx.
	meshUnreachable
	// meshUnserved is no mesh at the network's address: a 404 or a name
	// that does not resolve. It also happens while a mesh starts, so only
	// the control service can say the network is gone.
	meshUnserved
	// meshCredentialRefused is the mesh refusing the node's credential, or
	// a stored credential that no trusted key verifies.
	meshCredentialRefused
	// meshKeyUntrusted is a mesh whose signing keys are vouched for by no
	// key the node trusts, as after a rotation it slept through.
	meshKeyUntrusted
)

func meshAnswerOf(err error) meshAnswer {
	if err == nil {
		return meshFine
	}
	msg := err.Error()
	var status *meshnode.ControlPlaneStatusError
	var refresh *meshnode.RefreshError
	var dns *net.DNSError
	switch {
	case strings.Contains(msg, "not signed by any trusted control plane key"):
		return meshKeyUntrusted
	case strings.Contains(msg, "loaded identity fails role requirement"),
		errors.Is(err, meshnode.ErrFatalAuth),
		errors.As(err, &status) && (status.Code == http.StatusUnauthorized || status.Code == http.StatusForbidden),
		errors.As(err, &refresh) && (refresh.StatusCode == http.StatusUnauthorized || refresh.StatusCode == http.StatusForbidden):
		return meshCredentialRefused
	case errors.Is(err, meshnode.ErrControlPlaneNotFound),
		errors.As(err, &dns) && dns.IsNotFound:
		return meshUnserved
	}
	return meshUnreachable
}

// suspect says whether an answer could mean the network no longer admits
// the node, so the control service is worth asking.
func (a meshAnswer) suspect() bool { return a >= meshUnserved }

// controlAnswer is what the control service says about the node's
// membership.
type controlAnswer int

const (
	// controlNoAnswer is no answer, or one that says nothing: an outage,
	// or a control service without the node-status endpoint.
	controlNoAnswer controlAnswer = iota
	controlMember
	controlRemoved
	controlUnknownNetwork
)

// askControl asks the control service, signed with the node's key, whether
// its network still admits it.
func askControl(ctx context.Context, control, network string, priv crypto.PrivKey) controlAnswer {
	if control == "" || network == "" {
		return controlNoAnswer
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		return controlNoAnswer
	}
	ts := time.Now().UnixMilli()
	sig, err := priv.Sign(nodeStatusPayload(network, pid.String(), ts))
	if err != nil {
		return controlNoAnswer
	}
	b, _ := json.Marshal(map[string]any{"network_id": network, "peer_id": pid.String(), "timestamp_ms": ts, "signature": base64.RawURLEncoding.EncodeToString(sig)})
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(control, "/")+"/v1/node-status", bytes.NewReader(b))
	if err != nil {
		return controlNoAnswer
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return controlNoAnswer
	}
	defer resp.Body.Close()
	var answer struct {
		Status string `json:"status"`
		Code   string `json:"code"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&answer)
	switch {
	case resp.StatusCode == http.StatusOK && answer.Status == "member":
		return controlMember
	case resp.StatusCode == http.StatusGone && answer.Code == "node_removed":
		return controlRemoved
	case resp.StatusCode == http.StatusNotFound && answer.Code == "unknown_network":
		return controlUnknownNetwork
	}
	return controlNoAnswer
}

// nodeStatusPayload is what the node signs to ask the control service about
// its membership.
func nodeStatusPayload(network, peerID string, unixMs int64) []byte {
	return fmt.Appendf(nil, "agentiscale-node-status\n%s\n%s\n%d", network, peerID, unixMs)
}

// diagnosis decides whether the network is gone for good.
type diagnosis struct {
	dataDir string
	control string
	network string
	priv    crypto.PrivKey
}

// check returns a halt when meshErr, the node's latest failure to reach its
// mesh, and the control service's answer together say the network no longer
// admits the node; nil means keep retrying. Only a definite answer halts:
// from the control service, that the network or the node's membership is
// gone, or from the mesh itself, that it refuses the node's credential or
// is signed with a key the node does not trust. No answer never does.
func (d diagnosis) check(ctx context.Context, meshErr error) *nodeHalt {
	mesh := meshAnswerOf(meshErr)
	var reason string
	switch askControl(ctx, d.control, d.network, d.priv) {
	case controlUnknownNetwork:
		reason = haltNetworkGone
	case controlRemoved:
		reason = haltRemoved
	default:
		switch mesh {
		case meshKeyUntrusted:
			reason = haltSigningKeyRejected
		case meshCredentialRefused:
			reason = haltCredentialRefused
		default:
			return nil
		}
	}
	h := newHalt(reason, d.network, d.dataDir)
	return &h
}

// networkProbe is the part of the running node the watch needs.
type networkProbe interface {
	IsConnected() bool
	SyncControlPlane(ctx context.Context) error
	RefreshEnrollment(ctx context.Context) error
}

// A sync can refuse an expired credential that /refresh still accepts. Give
// that recovery a chance before treating the refusal as final. If refresh
// cannot answer, diagnose that failure alone: the original 401 does not make
// a timeout or a 5xx into a permanent refusal.
func syncForDiagnosis(ctx context.Context, node networkProbe) error {
	err := node.SyncControlPlane(ctx)
	if meshAnswerOf(err) != meshCredentialRefused {
		return err
	}
	if err := node.RefreshEnrollment(ctx); err != nil {
		return fmt.Errorf("refresh enrollment after sync refusal: %w", err)
	}
	log.Print("refreshed node credential after a sync refusal")
	return node.SyncControlPlane(ctx)
}

// How often the watch looks at the node's connection, how long the node may
// be cut off before it asks why, and how often it asks again; tests shorten
// them.
var (
	watchPoll       = 15 * time.Second
	watchCheckAfter = 2 * time.Minute
	watchCheckEvery = 5 * time.Minute
)

// watchNetwork asks why while the node is cut off from its network, and
// sends a halt once the answer is final.
func watchNetwork(ctx context.Context, node networkProbe, d diagnosis) <-chan nodeHalt {
	halted := make(chan nodeHalt, 1)
	go func() {
		var cutOffSince, lastCheck time.Time
		ticker := time.NewTicker(watchPoll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if node.IsConnected() {
				cutOffSince = time.Time{}
				continue
			}
			now := time.Now()
			if cutOffSince.IsZero() {
				cutOffSince = now
			}
			if now.Sub(cutOffSince) < watchCheckAfter || (!lastCheck.IsZero() && now.Sub(lastCheck) < watchCheckEvery) {
				continue
			}
			lastCheck = now
			err := syncForDiagnosis(ctx, node)
			if ctx.Err() != nil {
				return
			}
			if h := d.check(ctx, err); h != nil {
				halted <- *h
				return
			}
			if err != nil {
				log.Printf("cannot reach network %s yet (%s); retrying", d.network, strings.ReplaceAll(err.Error(), "\n", "; "))
			} else {
				log.Printf("network %s answers but no router has admitted this node yet; retrying", d.network)
			}
		}
	}()
	return halted
}

// waitHalted is a halted node: it says why once and waits to be stopped,
// so that a background service that restarts it does not retry either.
func waitHalted(ctx context.Context, h nodeHalt) error {
	log.Print(h.Message)
	<-ctx.Done()
	return nil
}

// halt records h and waits as a halted node.
func halt(ctx context.Context, dataDir string, h nodeHalt) error {
	if err := saveHalt(dataDir, h); err != nil {
		log.Printf("save %s: %v", haltFile, err)
	}
	return waitHalted(ctx, h)
}
