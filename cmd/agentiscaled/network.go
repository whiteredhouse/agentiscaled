package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// networkServerName is the name under which a hosted run gets the network's
// agents as an MCP server.
const networkServerName = "agentiscale-network"

// networkTokenEnv carries a run's token to a runtime that reads its MCP
// credential from the environment (Codex).
const networkTokenEnv = "AGENTISCALE_NETWORK_TOKEN"

// networkScope is all a run's token allows: A2A calls to agents.
const networkScope = "a2a://*"

// networkAccess is a hosted run's way to the network's other agents: an MCP
// server on loopback and the bearer token it takes.
type networkAccess struct {
	URL   string
	Token string
}

const networkInstructions = `agentiscale-network: the other agents of the network this agent serves in. Use list_agents to find them, get_agent_card to read what one does, send_task to give it a task, get_task (with wait_seconds) to follow the task until it ends (completed, rejected, cancelled, timeout or failed), and cancel_task to stop it. Use the same service and peer_id (omitted for an agent on this node) for every call about a task. A task in state input_required waits for your answer to the agent's question (its message): answer with send_task, giving the task's task_id. These tools work only while your own task runs, and an agent you call cannot call further agents.`

// taskNetwork gives each turn of a hosted agent's task its own way to the
// network: the node mints a sealed token that allows only A2A calls and
// expires with the turn, and this MCP server on loopback takes it as a bearer
// credential. Every call it makes for the run carries the token to the node,
// which checks it (signature, expiry, revocation, the A2A-only rule) before
// the call leaves for another node or reaches a local agent, and sends it on
// in place of the node's own credential. When the turn ends the token is
// revoked at the node and forgotten here. The run never gets the node's
// socket or credential.
type taskNetwork struct {
	// socket is the node API's Unix socket, agentSocket the local agent
	// API's.
	socket, agentSocket string
	url                 string
	// discover lists the A2A agents of other nodes.
	discover func(context.Context) ([]*api.DiscoveredProvider, error)
	node     *http.Client

	// calls are the tool calls being served, of every grant.
	calls sync.WaitGroup

	mu     sync.Mutex
	grants map[[sha256.Size]byte]*networkGrant
	// sent holds, for each running task of a hosted agent, the tasks it has
	// sent, across the turns of the task and so across their tokens. The
	// node is the sender of all of them, as of its owner's own tasks and
	// those of its other agents, so the agent may follow, answer or cancel
	// only these.
	sent map[a2a.TaskID]*sentTasks
}

// networkGrant is one turn's token and the MCP server it opens.
type networkGrant struct {
	expires time.Time
	handler *mcp.StreamableHTTPHandler
	close   func()
}

// sentTasks are the tasks one task of a hosted agent has sent, and the sends
// still on their way.
type sentTasks struct {
	mu      sync.Mutex
	tasks   map[sentTask]bool
	sending int
}

// sentTask names a task by the agent it went to and the ID that agent gave
// it: each agent makes its own IDs, so two may give the same one.
type sentTask struct {
	target taskTarget
	id     string
}

// maxSentTasks bounds the tasks one task of a hosted agent may send.
const maxSentTasks = 64

// reserve takes a place for one more task before it is sent, so that no
// send goes out past the bound, however many run at once. sent records the
// task the send created; a send that created none must call drop instead.
func (s *sentTasks) reserve() (sent func(id string, target taskTarget), drop func(), err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.tasks)+s.sending >= maxSentTasks {
		return nil, nil, fmt.Errorf("refused: your task may send at most %d tasks", maxSentTasks)
	}
	s.sending++
	var once sync.Once
	end := func(record func()) {
		once.Do(func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.sending--
			record()
		})
	}
	return func(id string, target taskTarget) { end(func() { s.tasks[sentTask{target, id}] = true }) },
		func() { end(func() {}) }, nil
}

// check says whether the task is one these were sent as, to target.
func (s *sentTasks) check(id string, target taskTarget) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.tasks[sentTask{target, id}] {
		return fmt.Errorf("task %s is not one you sent to %s; use the task_id, service and peer_id of a task your send_task created", id, target.Service)
	}
	return nil
}

// serveTaskNetwork serves the network's agents to hosted runs on a loopback
// port until ctx ends.
func serveTaskNetwork(ctx context.Context, socket, agentSocket string, discover func(context.Context) ([]*api.DiscoveredProvider, error)) (*taskNetwork, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	n := &taskNetwork{
		socket: socket, agentSocket: agentSocket, discover: discover,
		url:    "http://" + ln.Addr().String() + "/mcp",
		node:   socketClient(socket),
		grants: map[[sha256.Size]byte]*networkGrant{},
		sent:   map[a2a.TaskID]*sentTasks{},
	}
	n.node.Timeout = 10 * time.Second
	srv := &http.Server{Handler: n, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("agent network API: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return n, nil
}

// ServeHTTP admits a request only with the bearer token of a turn that is
// still running.
func (n *taskNetwork) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" || r.URL.Path != "/mcp" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := sha256.Sum256([]byte(bearer))
	n.mu.Lock()
	g := n.grants[key]
	expired := g != nil && !time.Now().Before(g.expires)
	n.mu.Unlock()
	if expired {
		g.close()
	}
	if g == nil || expired {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	g.handler.ServeHTTP(w, r)
}

// grant mints a token for a turn of agent's task that may run for at most
// ttl, whose calls go out at delegation depth depth, and opens the network to
// it. release revokes the token; call it when the turn ends, and forget when
// the task ends.
func (n *taskNetwork) grant(ctx context.Context, agent string, task a2a.TaskID, ttl time.Duration, depth int) (*networkAccess, func(), error) {
	if ttl <= 0 {
		return nil, nil, errors.New("no running time left")
	}
	expires := time.Now().Add(ttl)
	token, err := n.mint(ctx, expires)
	if err != nil {
		return nil, nil, fmt.Errorf("mint the task's network token: %w", err)
	}
	access, closeGrant := n.open(token, agent, n.sentBy(task), expires, depth)
	release := func() {
		// Closing does not wait for calls in flight: it cancels them.
		closeGrant()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := n.revoke(ctx, token); err != nil {
			// It still expires with the turn's deadline.
			log.Printf("agent %s: revoke the task's network token: %v", agent, err)
		}
	}
	return access, release, nil
}

// sentBy is the record of the tasks task has sent.
func (n *taskNetwork) sentBy(task a2a.TaskID) *sentTasks {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := n.sent[task]
	if s == nil {
		s = &sentTasks{tasks: map[sentTask]bool{}}
		n.sent[task] = s
	}
	return s
}

// forget drops the record of the tasks task has sent, once it has ended.
func (n *taskNetwork) forget(task a2a.TaskID) {
	if n == nil {
		return
	}
	n.mu.Lock()
	delete(n.sent, task)
	n.mu.Unlock()
}

// open serves the network's agents to the bearer of token until expires or
// until close, with the token as the caller's credential for every call, and
// only the tasks in sent to follow, answer or cancel.
func (n *taskNetwork) open(token, agent string, sent *sentTasks, expires time.Time, depth int) (access *networkAccess, close func()) {
	// Every call made for the grant ends with it, also one stalled on a
	// peer.
	life, endLife := context.WithCancel(context.Background())
	tasks := &a2aTasks{
		http: meshTokenClient(life, n.socket, token), base: "http://agentiscaled",
		local: meshTokenClient(life, n.agentSocket, token),
		depth: depth, self: agent, sent: sent,
	}
	server := mcp.NewServer(&mcp.Implementation{Name: networkServerName, Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: networkInstructions})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents of your network that you can send tasks to: those on this node (no peer_id) and those on other nodes that are online now (with their peer_id).",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listAgentsOut, error) {
		return n.listAgents(ctx, tasks)
	})
	addA2ATools(server, tasks)
	// A call ends with the grant, also one waiting for a task to settle.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			n.calls.Add(1)
			defer n.calls.Done()
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			defer context.AfterFunc(life, cancel)()
			return next(ctx, method, req)
		}
	})
	key := sha256.Sum256([]byte(token))
	// Sessions a runtime leaves open end with the grant, or when idle for
	// longer than a task may run.
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{SessionTimeout: taskDeadline})
	closeGrant := func() {
		n.mu.Lock()
		delete(n.grants, key)
		n.mu.Unlock()
		endLife()
		// A session closes once its calls have returned, which their
		// cancelled requests make them do; nothing waits for that.
		go func() {
			for session := range server.Sessions() {
				_ = session.Close()
			}
		}()
	}
	n.mu.Lock()
	n.grants[key] = &networkGrant{expires: expires, handler: handler, close: closeGrant}
	n.mu.Unlock()
	return &networkAccess{URL: n.url, Token: token}, closeGrant
}

// mint asks the node for its own credential narrowed to A2A calls until
// expires and sealed, so that the run cannot narrow it into anything else
// (RFC 8693 token exchange at the node's /oauth/token, which needs no
// control plane).
func (n *taskNetwork) mint(ctx context.Context, expires time.Time) (string, error) {
	rule, err := protojson.Marshal(&api.TaskAuthorizationRule{
		Name:       "agentiscale-task",
		ExpireTime: timestamppb.New(expires),
		Rules:      []*api.TaskRule{{AllowedServices: []string{networkScope}}},
	})
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {api.GrantTypeTokenExchange}, "options": {string(rule)}, "seal": {"true"}}
	body, err := n.post(ctx, "/oauth/token", form)
	if err != nil {
		return "", err
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.AccessToken == "" {
		return "", errors.New("the node answered without a token")
	}
	return out.AccessToken, nil
}

// revoke makes the node refuse the token from now on (RFC 7009).
func (n *taskNetwork) revoke(ctx context.Context, token string) error {
	_, err := n.post(ctx, "/oauth/revoke", url.Values{"token": {token}})
	return err
}

func (n *taskNetwork) post(ctx context.Context, path string, form url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://agentiscaled"+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := n.node.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error       string `json:"error"`
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(body, &oauthErr)
		return nil, fmt.Errorf("%s: HTTP %d %s %s", path, resp.StatusCode, oauthErr.Error, oauthErr.Description)
	}
	return body, nil
}

// agentRow is one agent list_agents found.
type agentRow struct {
	Service     string `json:"service"`
	PeerID      string `json:"peer_id,omitempty"`
	Description string `json:"description,omitempty"`
}

type listAgentsOut struct {
	Agents []agentRow `json:"agents"`
}

func (n *taskNetwork) listAgents(ctx context.Context, tasks *a2aTasks) (*mcp.CallToolResult, listAgentsOut, error) {
	var out listAgentsOut
	info, err := tasks.localInfo(ctx)
	if err != nil {
		return nil, out, fmt.Errorf("list this node's agents: %w", err)
	}
	for _, name := range info.Services {
		if name != tasks.self {
			out.Agents = append(out.Agents, agentRow{Service: name})
		}
	}
	if n.discover != nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		providers, err := n.discover(ctx)
		if err != nil {
			return nil, out, fmt.Errorf("discover the network's agents: %w", err)
		}
		for _, p := range providers {
			row := agentRow{Service: p.GetSrvName(), PeerID: p.GetPeerId(), Description: p.GetSrvDescription()}
			if p.GetPeerId() == info.PeerID || !serviceName.MatchString(row.Service) || slices.Contains(out.Agents, row) {
				continue
			}
			out.Agents = append(out.Agents, row)
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

// meshTokenClient dials socket and presents token on every request as the
// caller's Agent Mesh credential, which the node checks and acts under. Each
// request ends when life does, its response included.
func meshTokenClient(life context.Context, socket, token string) *http.Client {
	c := socketClient(socket)
	c.Transport = meshToken{c.Transport, token, life}
	return c
}

type meshToken struct {
	next  http.RoundTripper
	token string
	life  context.Context
}

func (m meshToken) RoundTrip(r *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(m.life, cancel)
	done := func() { stop(); cancel() }
	r = r.Clone(ctx)
	r.Header.Set(api.HeaderMeshAuthentication, "Bearer "+m.token)
	resp, err := m.next.RoundTrip(r)
	if err != nil {
		done()
		return nil, err
	}
	resp.Body = &bodyThen{ReadCloser: resp.Body, then: done}
	return resp, nil
}

// bodyThen calls then once the body is closed.
type bodyThen struct {
	io.ReadCloser
	then func()
}

func (b *bodyThen) Close() error {
	err := b.ReadCloser.Close()
	b.then()
	return err
}

// wait waits for the tool calls being served to return.
func (n *taskNetwork) wait() { n.calls.Wait() }
