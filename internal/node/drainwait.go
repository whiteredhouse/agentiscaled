package node

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"

	"github.com/whiteredhouse/agentiscaled/internal/caller"
)

// The node answers agent drain -wait itself: it owns the drains and the
// task state, so it judges both under its own locks and clock, and the
// command only asks and waits.

// drainWaitRequest is the body of POST /agents/drain/wait: the drains to
// wait on, each named by its agent and known by when it began, as the
// drain file the command saved says. WithinMS, when set, is how long the
// command can wait for this answer, so the node answers within it.
type drainWaitRequest struct {
	Agents   map[caller.AgentName]time.Time `json:"agents"`
	WithinMS int64                          `json:"within_ms,omitempty"`
}

// What the node answers about the drains a command waits on.
const (
	drainHeld    = "drained" // every drain holds and no agent has a task
	drainWorking = "working" // every drain holds, and Left still have tasks
	drainLifted  = "lifted"  // Agent's drain was lifted, or lifted and set again
)

// drainWaitAnswer is the node's answer to POST /agents/drain/wait.
type drainWaitAnswer struct {
	State string   `json:"state"`
	Left  []string `json:"left,omitempty"`
	Agent string   `json:"agent,omitempty"`
}

// drainWaitLongest is how long the node holds a request before it answers
// that work remains, so that the command hears from it now and then.
var drainWaitLongest = 20 * time.Second

// drainWaitEveryCheck is how often the node checks while it holds one.
var drainWaitEveryCheck = 100 * time.Millisecond

// drainState judges the drains at one moment. The agents it serves are
// read under all their locks with one clock reading, as admission reads
// the drain under the same lock before it takes the running slot; an agent
// the node does not serve has no task, and only its file says whether its
// drain still holds.
func drainState(agents []*agentService, dataDir string, held map[caller.AgentName]time.Time) drainWaitAnswer {
	served := map[caller.AgentName]agentActivity{}
	var named []*agentService
	for _, s := range agents {
		if _, ok := held[s.name]; ok {
			named = append(named, s)
		}
	}
	for i, a := range agentsActivity(named) {
		served[named[i].name] = a
	}
	answer := drainWaitAnswer{State: drainHeld}
	for _, name := range slices.Sorted(maps.Keys(held)) {
		since := held[name]
		a, ok := served[name]
		var drain *drainMark
		if ok {
			drain = a.Drain
		} else {
			drain = loadDrain(drainPath(dataDir, name.String()))
		}
		if drain == nil || !drain.Since.Equal(since) {
			return drainWaitAnswer{State: drainLifted, Agent: name.String()}
		}
		if ok && (a.State == activityBusy || a.Waiting > 0) {
			answer.State = drainWorking
			answer.Left = append(answer.Left, name.String())
		}
	}
	return answer
}

// waitDrainsHeld answers once the drains hold and no agent has a task, a
// drain is lifted, ctx ends or longest passes; it checks every so often.
func waitDrainsHeld(ctx context.Context, agents []*agentService, dataDir string, held map[caller.AgentName]time.Time, longest, every time.Duration) drainWaitAnswer {
	ctx, cancel := context.WithTimeout(ctx, longest)
	defer cancel()
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		answer := drainState(agents, dataDir, held)
		if answer.State != drainWorking {
			return answer
		}
		select {
		case <-ctx.Done():
			return answer
		case <-tick.C:
		}
	}
}

// serveDrainWait is POST /agents/drain/wait on the owner's local socket.
func serveDrainWait(w http.ResponseWriter, r *http.Request, agents []*agentService, dataDir string) {
	serveDrainWaitEvery(w, r, agents, dataDir, drainWaitLongest, drainWaitEveryCheck)
}

// serveDrainWaitEvery is serveDrainWait holding a request up to longest
// and checking every so often.
func serveDrainWaitEvery(w http.ResponseWriter, r *http.Request, agents []*agentService, dataDir string, longest, every time.Duration) {
	// Decoding parses each name (caller.AgentName).
	var req drainWaitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil || len(req.Agents) == 0 {
		http.Error(w, "the request names no drain of an agent", http.StatusBadRequest)
		return
	}
	if within := time.Duration(req.WithinMS) * time.Millisecond; req.WithinMS > 0 && within < longest {
		longest = within
	}
	answer := waitDrainsHeld(r.Context(), agents, dataDir, req.Agents, longest, every)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(answer)
}
