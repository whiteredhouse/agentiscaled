// Copyright 2026 Google LLC
// Modifications copyright 2026 agentiscale.
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

// Package node exports the agentmesh-node runtime (internal/node) so that a
// program outside this module can embed a node instead of running the
// agentmesh-node binary.
//
// This file is agentiscale's only change to the upstream source. It adds no
// behaviour: every name is an alias of the internal package, so the fork
// stays a plain copy of upstream plus this directory.
package node

import (
	cpclient "github.com/google/agentmesh/internal/controlplane/client"
	"github.com/google/agentmesh/internal/identity"
	"github.com/google/agentmesh/internal/logging"
	inode "github.com/google/agentmesh/internal/node"
)

type (
	AgentMeshNode      = inode.AgentMeshNode
	Options            = inode.Options
	Store              = inode.Store
	NodeConfigComplete = inode.NodeConfigComplete
	Reachability       = inode.Reachability
	TokenSource        = inode.TokenSource
	TokenSourceConfig  = inode.TokenSourceConfig

	// ControlPlaneStatusError is a control plane answer other than success,
	// with its status code.
	ControlPlaneStatusError = cpclient.StatusError
)

const (
	ReachabilityPrivate = inode.ReachabilityPrivate
	ReachabilityAuto    = inode.ReachabilityAuto

	DefaultDiscoveryInterval        = inode.DefaultDiscoveryInterval
	DefaultConfigFile               = inode.DefaultConfigFile
	DefaultRouterConnectTimeout     = inode.DefaultRouterConnectTimeout
	DefaultRouters                  = inode.DefaultRouters
	DefaultSocketName               = inode.DefaultSocketName
	DefaultControlPlaneSyncInterval = inode.DefaultControlPlaneSyncInterval
	DefaultSecretsDir               = inode.DefaultSecretsDir
	StoreFile                       = inode.StoreFile
)

// LoggingOptions and SetupLogging configure the node's logger the way the
// agentmesh-node binary does.
type LoggingOptions = logging.Options

var SetupLogging = logging.Setup

var (
	NewAgentMeshNode         = inode.NewAgentMeshNode
	NewStore                 = inode.NewStore
	LoadNodeConfig           = inode.LoadNodeConfig
	GetOrGenerateKey         = inode.GetOrGenerateKey
	ResolveTokenSource       = inode.ResolveTokenSource
	StartSidecarServer       = inode.StartSidecarServer
	StartUnauthSidecarServer = inode.StartUnauthSidecarServer
	StartMetricsServer       = inode.StartMetricsServer
)

var (
	// ErrControlPlaneNotFound marks a 404 from the control plane.
	ErrControlPlaneNotFound = cpclient.ErrNotFound
	// ErrFatalAuth marks a router's refusal of the node's credential.
	ErrFatalAuth = inode.ErrFatalAuth
	// MintBootstrapBiscuitToken signs a node credential, as a control plane
	// does on enrollment; tests use it to stand in for a mesh.
	MintBootstrapBiscuitToken = identity.MintBootstrapBiscuitToken
)
