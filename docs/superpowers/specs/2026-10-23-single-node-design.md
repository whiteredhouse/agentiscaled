# One node by default

Approved in the conversation: the default node connects local clients and hosts one offered agent. It has one mesh identity, enrollment, service and recovery target. Explicit alternate data directories remain independent; existing nodes are never adopted, migrated or removed automatically.

The wizard configures the current node instead of invoking a second `up`. Repeating `up` preserves saved settings unless a corresponding flag overrides them. Runtime sign-in precedes starting the service. Local clients connect by default even when the node hosts an agent.

A2A discovery includes this node's registered agents. Requests to them use the ordinary egress checks and the same ingress handler as remote requests, with the actual local peer as caller. Local dispatch must stream and propagate cancellation. Provider identity, publish grants, caller authorization, labels and revocation must remain enforced. MCP tool discovery retains its existing remote-only scope.

Acceptance: local discovery and A2A invocation succeed without a second peer; restart and reset preserve agent settings; repeated setup creates no second node; explicit separate nodes remain supported.
