package node

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestAgyCallsTheNetwork runs the real Antigravity CLI, given by
// AGENTISCALE_TEST_AGY, signed in as the owner's own agy is, with a turn's
// network in front of the stand-in node (fakeNodeNetworkServer). agy must
// start the network bridge from the run's folder, the bridge must spend its
// ticket, and the call must reach the node with the turn's token and no
// approval prompt. A second task asks a question and resumes its
// conversation with the answer. Each run leaves agy conversations in the
// owner's agy data and uses their quota.
//
//	AGENTISCALE_TEST_AGY=$(command -v agy) go test ./internal/node -run TestAgyCallsTheNetwork -v
func TestAgyCallsTheNetwork(t *testing.T) {
	command := os.Getenv("AGENTISCALE_TEST_AGY")
	if command == "" {
		t.Skip("set AGENTISCALE_TEST_AGY to a signed-in Antigravity CLI to run")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := bridgeExecutable
	bridgeExecutable = func() (string, error) { return self, nil }
	t.Cleanup(func() { bridgeExecutable = old })
	t.Setenv("AGENTISCALED_TEST_BRIDGE", "1")
	rt, err := newAgyRuntime(command, "", agentPolicy{DirInstructions: true, DirSkills: true, OwnerConfig: true})
	if err != nil {
		t.Fatal(err)
	}
	agy := rt.(*agyRuntime)

	t.Run("network", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		network, calls := fakeNodeNetworkServer(t)
		g, err := network.grantFor("caller", "t-agy")
		if err != nil {
			t.Fatal(err)
		}
		defer g.begin(ctx, "turn-token", time.Now().Add(5*time.Minute))()
		access := &networkAccess{URL: network.url, Token: g.bearer, ticket: func() (string, error) { return network.ticket(g) }}
		var steps []string
		out, err := agy.Run(ctx, t.TempDir(), taskRequest{Sender: testSender, Network: access,
			Text:     "Call the list_agents tool of the agentiscale-network MCP server and answer with the names of the agents it lists, and nothing else.",
			Progress: func(s string) { steps = append(steps, s) }})
		t.Logf("steps %q, answer %+v", steps, out)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.Text, "helper") {
			t.Errorf("answer %q does not name the helper", out.Text)
		}
		if got := calls(); !strings.Contains(got, "GET /info Bearer turn-token") {
			t.Errorf("the node got %q", got)
		}
	})

	t.Run("question", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		dir := t.TempDir()
		out, err := agy.Run(ctx, dir, taskRequest{Sender: testSender,
			Text: "Before you answer, ask me which number I picked: answer with needs_input and the question. Once I tell you, answer with the number doubled."})
		t.Logf("first turn %+v", out)
		if err != nil {
			t.Fatal(err)
		}
		if !out.Question || out.Session == "" {
			t.Fatalf("first turn %+v is not a question", out)
		}
		out, err = agy.Run(ctx, dir, taskRequest{Sender: testSender, Text: "21", Session: out.Session})
		t.Logf("second turn %+v", out)
		if err != nil {
			t.Fatal(err)
		}
		if out.Question || !strings.Contains(out.Text, "42") {
			t.Errorf("answer %+v", out)
		}
	})
}
