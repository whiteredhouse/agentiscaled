package main

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/agentmesh/api"
)

// A hosted agent's token (taskNetwork) names the task it works for and the
// delegation depth of the tasks it sends: "agentiscale-task/<depth>/<task
// ID>". The name is in the token's task rule, which the token's holder
// cannot change, since the token is sealed. The node that receives a call
// verifies the token and passes the name on to the agent as X-Mesh-Task,
// so what a task's sender says of it comes from the token, not from the
// message.
const taskClaimPrefix = "agentiscale-task/"

// maxTaskClaim is the longest name a task rule may have.
const maxTaskClaim = 128

var errNoSender = errors.New("the request carries no authenticated sender")

// taskClaim is the task rule name of a token for a turn of task, whose
// calls go out at depth.
func taskClaim(depth int, task a2a.TaskID) (string, error) {
	name := taskClaimPrefix + strconv.Itoa(depth) + "/" + string(task)
	if len(name) > maxTaskClaim || !printable(string(task)) || task == "" {
		return "", fmt.Errorf("task ID %q cannot be named in a token", task)
	}
	return name, nil
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// taskSender is who sent a request to an agent: the node the mesh
// authenticated, and the task its token was narrowed to, if any.
type taskSender struct {
	peer string
	// task is the name of the token's task rule; empty for the node's
	// own credential.
	task string
	// depth is how far the request has been delegated: 0 from a person,
	// and from a hosted agent the depth its token names.
	depth int
}

// senderOf reads the sender the node stamped on a request. A request that
// did not come through the mesh has none.
func senderOf(p *a2asrv.ServiceParams) (taskSender, error) {
	c := taskSender{peer: firstParam(p, api.HeaderPeerID), task: firstParam(p, api.HeaderMeshTask)}
	if c.peer == "" {
		return taskSender{}, errNoSender
	}
	rest, ok := strings.CutPrefix(c.task, taskClaimPrefix)
	if !ok {
		// The node's own credential, or one narrowed by other means than
		// a hosted agent's: a person's.
		return c, nil
	}
	depth, task, ok := strings.Cut(rest, "/")
	d, err := strconv.Atoi(depth)
	if !ok || err != nil || d < 0 || depth != strconv.Itoa(d) || task == "" {
		return taskSender{}, fmt.Errorf("malformed delegation depth in %q", c.task)
	}
	c.depth = d
	return c, nil
}

// String is the sender as the agent keeps it: the tasks a hosted agent sends
// are its task's alone, not its node's, and the node's are not its own.
func (c taskSender) String() string {
	if c.task == "" {
		return c.peer
	}
	return c.peer + " " + c.task
}
