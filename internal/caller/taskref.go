package caller

import (
	"fmt"
	"strings"

	"github.com/libp2p/go-libp2p/core/peer"
)

// TaskRef says where a task this node sent is: the node that offers its
// agent, the agent's name and the task's ID, written PEER/AGENT/TASK.
// task send prints it; task wait, task cancel and an answer take it, so a
// task is found where it went, whichever agent of that name would take a
// new task now.
type TaskRef struct {
	Target
	ID string
}

// String is the reference as task send prints it.
func (r TaskRef) String() string {
	return r.PeerID + "/" + r.Service + "/" + r.ID
}

// ParseTaskRef reads a reference task send printed.
func ParseTaskRef(s string) (TaskRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return TaskRef{}, fmt.Errorf("%q is not a task reference: task send prints one as PEER/AGENT/TASK", s)
	}
	if _, err := peer.Decode(parts[0]); err != nil {
		return TaskRef{}, fmt.Errorf("%q is not a task reference: %q is not a peer ID", s, parts[0])
	}
	if !ValidAgentName(parts[1]) {
		return TaskRef{}, fmt.Errorf("%q is not a task reference: %q is not an agent name", s, parts[1])
	}
	if !taskID.MatchString(parts[2]) {
		return TaskRef{}, fmt.Errorf("%q is not a task reference: %q is not a task ID", s, parts[2])
	}
	return TaskRef{Target{PeerID: parts[0], Service: parts[1]}, parts[2]}, nil
}
