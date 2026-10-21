// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// What an operator running a PEP needs to see: which destinations this node
// was told to serve and serves, how each request was decided, and whether
// the credential the platform was supposed to deliver is there. Decisions
// are labelled by destination and outcome and not by caller, so the series
// count stays bounded by the policy document and not by the fleet.
var (
	egressDecisionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentmesh_node_egress_decisions_total",
			Help: "Requests for egress destinations this node serves, by destination and outcome (allow, deny, not_assigned, credential_unavailable)",
		},
		[]string{"destination", "outcome"},
	)

	egressAssignmentsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentmesh_node_egress_assignments_total",
			Help: "Egress assignments applied from the control plane, by outcome (registered, withdrawn, refused)",
		},
		[]string{"outcome"},
	)
)

const (
	egressOutcomeAllow                 = "allow"
	egressOutcomeDeny                  = "deny"
	egressOutcomeNotAssigned           = "not_assigned"
	egressOutcomeCredentialUnavailable = "credential_unavailable"
)

func recordEgressDecision(destination, outcome string) {
	egressDecisionsTotal.WithLabelValues(destination, outcome).Inc()
}

func recordEgressAssignment(outcome string, count int) {
	if count > 0 {
		egressAssignmentsTotal.WithLabelValues(outcome).Add(float64(count))
	}
}
