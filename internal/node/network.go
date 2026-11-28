package node

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/google/agentmesh/api"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/whiteredhouse/agentiscaled/internal/caller"
	"github.com/whiteredhouse/agentiscaled/internal/nodeclient"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// networkServerName is the name under which a hosted run gets the network's
// agents as an MCP server.
const networkServerName = "agentiscale-network"

// networkTokenEnv carries a run's token to a runtime that reads its MCP
// credential from the environment (Codex).
const networkTokenEnv = "AGENTISCALE_NETWORK_TOKEN"

// networkTicketEnv is where an ACP agent's network bridge finds its
// one-time ticket (RunNetworkBridge).
const networkTicketEnv = "AGENTISCALE_NETWORK_TICKET"

// networkScope is all a run's token allows: A2A calls to agents.
const networkScope = "a2a://*"

// networkAccess is a hosted run's way to the network's other agents: an MCP
// server on loopback and the bearer token it takes.
type networkAccess struct {
	URL   string
	Token string
	// ticket, when set, mints a one-time ticket that the network bridge
	// (agentiscaled network-bridge) exchanges for a bearer of its own that
	// reaches the same grant as Token, for a runtime that can start an MCP
	// server but has no place to keep a credential that the agent cannot
	// read (agy). revoke drops the ticket and that bearer; until then the
	// bearer lasts as long as the grant.
	ticket func() (ticket string, revoke func(), err error)
}

// networkTicketTTL bounds how long a ticket waits for the bridge.
const networkTicketTTL = 2 * time.Minute

// networkTicket is a one-time secret for a bearer of its own (alias) that
// reaches grant.
type networkTicket struct {
	alias   string
	grant   *networkGrant
	expires time.Time
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

	mu sync.Mutex
	// grants are the tasks' grants by the hash of their bearers, and
	// byTask the same grants by their tasks.
	grants map[[sha256.Size]byte]*networkGrant
	byTask map[a2a.TaskID]*networkGrant
	// tickets are the one-time tickets not yet redeemed, by their hashes.
	tickets map[[sha256.Size]byte]networkTicket
	// sent holds, for each running task of a hosted agent, the tasks it has
	// sent, across the turns of the task and so across their tokens. The
	// node is the sender of all of them, as of its owner's own tasks and
	// those of its other agents, so the agent may follow, answer or cancel
	// only these.
	sent map[a2a.TaskID]*sentTasks
}

// networkGrant is the network of one task of a hosted agent over all its
// turns: one MCP server, which the run reaches with the grant's bearer, a
// secret of this server's that is no credential anywhere else. A turn opens
// it with the turn's token (begin) until the turn ends; between turns the
// server refuses requests and has no token to call with, and calls in
// flight end with the turn. So a run whose process outlives its turn, such
// as an ACP agent waiting for an answer, keeps its MCP session, and its
// next turn's calls carry that turn's token.
type networkGrant struct {
	bearer  string
	handler http.Handler
	server  *mcp.Server

	mu   sync.Mutex
	turn *grantTurn // nil between turns
	// opening holds requests that can create an MCP session. It becomes nil
	// when the grant closes, so an already admitted HTTP request cannot reopen it.
	opening map[*http.ResponseController]struct{}
}

// grantTurn is the turn that holds a grant open.
type grantTurn struct {
	token   string
	expires time.Time
	life    context.Context
	end     context.CancelFunc
}

// current is the turn holding g open, nil when it ended or its token expired.
func (g *networkGrant) current() *grantTurn {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.turn != nil && (g.turn.life.Err() != nil || !time.Now().Before(g.turn.expires)) {
		g.turn.end()
		g.turn = nil
	}
	return g.turn
}

// begin opens g until ctx ends, the token expires, or the run releases it.
func (g *networkGrant) begin(ctx context.Context, token string, expires time.Time) (end func()) {
	life, cancel := context.WithDeadline(ctx, expires)
	t := &grantTurn{token: token, expires: expires, life: life, end: cancel}
	g.mu.Lock()
	if g.turn != nil {
		g.turn.end()
	}
	g.turn = t
	g.mu.Unlock()
	return func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		// Ending does not wait for calls in flight: it cancels them.
		cancel()
		if g.turn == t {
			g.turn = nil
		}
	}
}

// close ends g for good: its turn and its MCP sessions.
func (g *networkGrant) close() {
	g.mu.Lock()
	if g.turn != nil {
		g.turn.end()
		g.turn = nil
	}
	for request := range g.opening {
		// Interrupt a handshake still reading its body or writing its response.
		_ = request.SetReadDeadline(time.Now())
		_ = request.SetWriteDeadline(time.Now())
	}
	g.opening = nil
	g.mu.Unlock()
	go g.closeSessions()
}

func (g *networkGrant) closeSessions() {
	for session := range g.server.Sessions() {
		_ = session.Close()
	}
}

func (g *networkGrant) serveHTTP(w http.ResponseWriter, r *http.Request) {
	// Only a POST without a session ID can open a persistent session.
	if r.Method == http.MethodPost && r.Header.Get("Mcp-Session-Id") == "" {
		request := http.NewResponseController(w)
		g.mu.Lock()
		if g.opening == nil {
			g.mu.Unlock()
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		g.opening[request] = struct{}{}
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			delete(g.opening, request)
			closed := g.opening == nil
			g.mu.Unlock()
			if closed {
				// The SDK may have registered this session after close took its snapshot.
				go g.closeSessions()
			}
		}()
	}
	g.handler.ServeHTTP(w, r)
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
	target caller.Target
	id     string
}

// maxSentTasks bounds the tasks one task of a hosted agent may send.
const maxSentTasks = 64

// Reserve takes a place for one more task before it is sent, so that no
// send goes out past the bound, however many run at once. sent records the
// task the send created; a send that created none must call drop instead.
func (s *sentTasks) Reserve() (sent func(id string, target caller.Target), drop func(), err error) {
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
	return func(id string, target caller.Target) { end(func() { s.tasks[sentTask{target, id}] = true }) },
		func() { end(func() {}) }, nil
}

// Check says whether the task is one these were sent as, to target.
func (s *sentTasks) Check(id string, target caller.Target) error {
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
		url:     "http://" + ln.Addr().String() + "/mcp",
		node:    nodeclient.SocketClient(socket),
		grants:  map[[sha256.Size]byte]*networkGrant{},
		byTask:  map[a2a.TaskID]*networkGrant{},
		sent:    map[a2a.TaskID]*sentTasks{},
		tickets: map[[sha256.Size]byte]networkTicket{},
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

// ServeHTTP admits a request only with the bearer of a task that has not
// ended; only a turn that runs can call a tool.
func (n *taskNetwork) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == networkTicketPath {
		n.redeem(w, r)
		return
	}
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" || r.URL.Path != "/mcp" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	key := sha256.Sum256([]byte(bearer))
	n.mu.Lock()
	g := n.grants[key]
	n.mu.Unlock()
	// Between turns the bearer still reaches its server, so that the run
	// keeps its MCP session, but no tool call runs (newGrant).
	if g == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	g.serveHTTP(w, r)
}

// grant mints a token for a turn of agent's task that may run for at most
// ttl, whose calls go out at delegation depth depth, and opens the network to
// it. release revokes the token; call it when the turn ends, and forget when
// the task ends.
//
// The task's endpoint (access) comes also when the turn gets no token
// (err), so that a run whose process lasts into a later turn, which may get
// one, knows it: its tool calls then fail until then. access is nil only
// when the endpoint itself cannot be made.
func (n *taskNetwork) grant(ctx context.Context, agent caller.AgentName, task a2a.TaskID, ttl time.Duration, depth int) (access *networkAccess, release func(), err error) {
	g, err := n.grantFor(agent, task)
	if err != nil {
		return nil, nil, err
	}
	access = &networkAccess{URL: n.url, Token: g.bearer, ticket: func() (string, func(), error) { return n.ticket(g) }}
	if ttl <= 0 {
		return access, func() {}, errors.New("no running time left")
	}
	claim, err := taskClaim(depth, task)
	if err != nil {
		return access, func() {}, err
	}
	expires := time.Now().Add(ttl)
	token, err := n.mint(ctx, claim, expires)
	if err != nil {
		return access, func() {}, fmt.Errorf("mint the task's network token: %w", err)
	}
	endTurn := g.begin(ctx, token, expires)
	release = func() {
		endTurn()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := n.revoke(ctx, token); err != nil {
			// It still expires with the turn's deadline.
			log.Printf("agent %s: revoke the task's network token: %v", agent, err)
		}
	}
	return access, release, nil
}

// grantFor is task's grant, made on its first turn.
func (n *taskNetwork) grantFor(agent caller.AgentName, task a2a.TaskID) (*networkGrant, error) {
	n.mu.Lock()
	g := n.byTask[task]
	n.mu.Unlock()
	if g != nil {
		return g, nil
	}
	g, err := n.newGrant(agent, n.sentBy(task))
	if err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.byTask == nil {
		n.byTask = map[a2a.TaskID]*networkGrant{}
	}
	if other := n.byTask[task]; other != nil {
		delete(n.grants, sha256.Sum256([]byte(g.bearer)))
		return other, nil
	}
	n.byTask[task] = g
	return g, nil
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
	g := n.byTask[task]
	delete(n.byTask, task)
	if g != nil {
		delete(n.grants, sha256.Sum256([]byte(g.bearer)))
		n.dropTickets(g)
	}
	n.mu.Unlock()
	if g != nil {
		g.close()
	}
}

// open serves the network's agents to a grant of its own for one turn with
// token, until expires or until close, with the token as the caller's
// credential for every call, and only the tasks in sent to follow, answer or
// cancel.
func (n *taskNetwork) open(token string, agent caller.AgentName, sent *sentTasks, expires time.Time) (access *networkAccess, close func()) {
	g, err := n.newGrant(agent, sent)
	if err != nil {
		// Only a failing source of randomness gets here.
		panic(err)
	}
	end := g.begin(context.Background(), token, expires)
	return &networkAccess{URL: n.url, Token: g.bearer, ticket: func() (string, func(), error) { return n.ticket(g) }}, func() {
		end()
		n.mu.Lock()
		delete(n.grants, sha256.Sum256([]byte(g.bearer)))
		n.dropTickets(g)
		n.mu.Unlock()
		g.close()
	}
}

// newGrant makes a grant of agent's whose tools may use only the tasks in
// sent, with no turn yet.
func (n *taskNetwork) newGrant(agent caller.AgentName, sent *sentTasks) (*networkGrant, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	g := &networkGrant{bearer: hex.EncodeToString(b), opening: make(map[*http.ResponseController]struct{})}
	tasks := &caller.Client{
		Mesh:    grantClient(n.socket),
		BaseURL: "http://agentiscaled",
		Local:   grantClient(n.agentSocket),
		Self:    agent,
		Sent:    sent,
	}
	server := mcp.NewServer(&mcp.Implementation{Name: networkServerName, Version: "0.1.0"},
		&mcp.ServerOptions{Instructions: networkInstructions})
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_agents",
		Description: "List the agents of your network that you can send tasks to: those on this node (no peer_id) and those on other nodes that are online now (with their peer_id). An agent with draining set refuses new tasks; its reason says why, and drained says it has no task left (without it, it still finishes the ones it has). runtime, when present, says what runs the agent, and model which model it asks for: \"\" when it runs its runtime's default.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listAgentsOut, error) {
		return n.listAgents(ctx, tasks)
	})
	caller.AddTools(server, tasks)
	// A call ends with its turn, also one waiting for a task to settle.
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			n.calls.Add(1)
			defer n.calls.Done()
			t := g.current()
			if t == nil {
				// The session's own methods still work, so that it
				// lasts into the task's next turn.
				if method == "tools/call" {
					return nil, errors.New("your task has no network access now: its turn has ended, or its token could not be made")
				}
				return next(ctx, method, req)
			}
			// The call is the admitting turn's: every request it makes
			// carries that turn's token and ends with it (grantToken),
			// also once a later turn holds the grant.
			ctx, cancel := context.WithCancel(context.WithValue(ctx, grantTurnKey{}, t))
			defer cancel()
			defer context.AfterFunc(t.life, cancel)()
			return next(ctx, method, req)
		}
	})
	g.server = server
	// A task may wait for several answers without making an MCP request.
	// Its grant closes the sessions when the task ends.
	g.handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	n.mu.Lock()
	n.grants[sha256.Sum256([]byte(g.bearer))] = g
	n.mu.Unlock()
	return g, nil
}

// mint asks the node for its own credential narrowed to A2A calls until
// expires, for the task claim names, and sealed, so that the run cannot
// narrow it into anything else or rename its task (RFC 8693 token exchange
// at the node's /oauth/token, which needs no control plane).
func (n *taskNetwork) mint(ctx context.Context, claim string, expires time.Time) (string, error) {
	rule, err := protojson.Marshal(&api.TaskAuthorizationRule{
		Name:       claim,
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

type agentRow = caller.AgentRow
type listAgentsOut = caller.AgentList

func (n *taskNetwork) listAgents(ctx context.Context, tasks *caller.Client) (*mcp.CallToolResult, listAgentsOut, error) {
	info, err := tasks.LocalInfo(ctx)
	if err != nil {
		return nil, listAgentsOut{}, fmt.Errorf("list this node's agents: %w", err)
	}
	var providers []*api.DiscoveredProvider
	if n.discover != nil {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		providers, err = n.discover(ctx)
		if err != nil {
			return nil, listAgentsOut{}, fmt.Errorf("discover the network's agents: %w", err)
		}
	}
	out := caller.ListedAgents(info, tasks.Self, providers)
	b, _ := json.MarshalIndent(out, "", "  ")
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, out, nil
}

// meshTokenClient dials socket and presents token on every request as the
// caller's Agent Mesh credential, which the node checks and acts under. Each
// request ends when life does, its response included.
func meshTokenClient(life context.Context, socket, token string) *http.Client {
	c := nodeclient.SocketClient(socket)
	c.Transport = meshToken{c.Transport, token, life}
	return c
}

type meshToken struct {
	next  http.RoundTripper
	token string
	life  context.Context
}

func (m meshToken) RoundTrip(r *http.Request) (*http.Response, error) {
	// A request made after its turn ended is not sent; cancellation
	// stops those already on their way.
	if m.life.Err() != nil {
		return nil, errors.New("the turn the request was made for has ended")
	}
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

// grantClient dials socket and presents, on every request, the token of
// the turn that admitted the tool call it is made for, and ends the
// request with that turn. A request made for no turn is not sent.
func grantClient(socket string) *http.Client {
	c := nodeclient.SocketClient(socket)
	c.Transport = grantToken{c.Transport}
	return c
}

// grantTurnKey keys the turn that admitted a tool call in its context.
type grantTurnKey struct{}

type grantToken struct{ next http.RoundTripper }

func (t grantToken) RoundTrip(r *http.Request) (*http.Response, error) {
	turn, _ := r.Context().Value(grantTurnKey{}).(*grantTurn)
	if turn == nil {
		return nil, errors.New("a network request made for no turn of the task")
	}
	if !time.Now().Before(turn.expires) {
		return nil, errors.New("the turn the request was made for has ended")
	}
	return meshToken{t.next, turn.token, turn.life}.RoundTrip(r)
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

// networkTicketPath is where the network bridge redeems its ticket, beside
// the MCP endpoint.
const networkTicketPath = "/ticket"

// ticketURL is the ticket endpoint of the network whose MCP endpoint is
// mcpURL.
func ticketURL(mcpURL string) string {
	return strings.TrimSuffix(mcpURL, "/mcp") + networkTicketPath
}

// ticket mints a one-time ticket for a bearer of its own that reaches g,
// which only the first redeem within networkTicketTTL gets, while g is
// open. revoke drops the ticket and that bearer, so that a bridge that
// outlives its turn cannot reach g again.
func (n *taskNetwork) ticket(g *networkGrant) (ticket string, revoke func(), err error) {
	b := make([]byte, 64)
	if _, err := rand.Read(b); err != nil {
		return "", nil, err
	}
	t, alias := hex.EncodeToString(b[:32]), hex.EncodeToString(b[32:])
	now := time.Now()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.tickets == nil {
		n.tickets = map[[sha256.Size]byte]networkTicket{}
	}
	for k, v := range n.tickets {
		if !now.Before(v.expires) {
			delete(n.tickets, k)
		}
	}
	key := sha256.Sum256([]byte(t))
	n.tickets[key] = networkTicket{alias: alias, grant: g, expires: now.Add(networkTicketTTL)}
	return t, func() {
		n.mu.Lock()
		delete(n.tickets, key)
		if aliasKey := sha256.Sum256([]byte(alias)); n.grants[aliasKey] == g {
			delete(n.grants, aliasKey)
		}
		n.mu.Unlock()
	}, nil
}

// dropTickets forgets g's tickets and the bearers they redeemed. n.mu is
// held.
func (n *taskNetwork) dropTickets(g *networkGrant) {
	for k, v := range n.tickets {
		if v.grant == g {
			delete(n.tickets, k)
		}
	}
	for k, v := range n.grants {
		if v == g {
			delete(n.grants, k)
		}
	}
}

// redeem answers a ticket (POST, form field ticket) with its own bearer for
// its grant, once: the ticket is spent whatever the answer.
func (n *taskNetwork) redeem(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// A client that stalls in the body does not hold the handler.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(5 * time.Second))
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	t := r.PostFormValue("ticket")
	key := sha256.Sum256([]byte(t))
	n.mu.Lock()
	v, ok := n.tickets[key]
	delete(n.tickets, key)
	if ok && (n.grants[sha256.Sum256([]byte(v.grant.bearer))] != v.grant || !time.Now().Before(v.expires)) {
		ok = false
	}
	if ok {
		n.grants[sha256.Sum256([]byte(v.alias))] = v.grant
	}
	n.mu.Unlock()
	if t == "" || !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, v.alias)
}
