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

var (
	inferenceTokensTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentmesh_node_inference_tokens_total",
			Help: "Total number of inference tokens tracked by agentmesh-node",
		},
		[]string{"peer_id", "model", "token_type"},
	)

	facadeRejectionsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "agentmesh_node_facade_rejections_total",
			Help: "Providers excluded by the OpenAI facade scorer, by reason",
		},
		[]string{"reason"},
	)

	facadeRetriesTotal = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "agentmesh_node_facade_retries_total",
			Help: "Completion attempts retried on another provider after a retryable failure",
		},
	)
)

// recordFacadeRejection accounts one excluded provider by reason.
func recordFacadeRejection(reason string) {
	facadeRejectionsTotal.WithLabelValues(reason).Inc()
}

func recordFacadeRetry() {
	facadeRetriesTotal.Inc()
}

// recordTokens increments the prompt and completion token metrics.
func recordTokens(peerID, model string, promptTokens, completionTokens int) {
	if model == "" {
		model = "unknown"
	}
	if peerID == "" {
		peerID = "unknown"
	}
	if promptTokens > 0 {
		inferenceTokensTotal.WithLabelValues(peerID, model, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		inferenceTokensTotal.WithLabelValues(peerID, model, "completion").Add(float64(completionTokens))
	}
}
