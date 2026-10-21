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

package api

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

// The directory parameters a mesh runs on when its control plane sets
// none (ControlPlaneInfoResponse.directory): three routers per key, a
// re-announcement every five minutes, and records that outlive three of
// them. Every member and router derives its settings from these three
// numbers, so a member started against no control plane at all behaves as
// one of a default mesh.
const (
	DefaultDHTReplication      = 3
	DefaultReprovideInterval   = 5 * time.Minute
	recordTTLReprovideMultiple = 3
)

// RecordTTL is how long a router keeps a record after its last
// announcement for a mesh that re-announces every reprovide: three
// intervals, so a provider that stopped without unpublishing leaves within
// it and two missed announcements do not unlist one that did not.
func RecordTTL(reprovide time.Duration) time.Duration {
	return recordTTLReprovideMultiple * reprovide
}

// NewDirectoryParameters is the message a control plane publishes for a
// replication factor and a reprovide interval, the TTL derived.
func NewDirectoryParameters(replication int, reprovide time.Duration) *DirectoryParameters {
	return &DirectoryParameters{
		Replication:       int32(replication),
		ReprovideInterval: durationpb.New(reprovide),
		RecordTtl:         durationpb.New(RecordTTL(reprovide)),
	}
}

// ValidateDirectoryParameters checks the two numbers an operator sets.
func ValidateDirectoryParameters(replication int, reprovide time.Duration) error {
	if replication < 1 {
		return fmt.Errorf("dht replication %d: at least one router must hold each record", replication)
	}
	if reprovide <= 0 {
		return fmt.Errorf("dht reprovide interval %s must be positive", reprovide)
	}
	return nil
}

// DirectorySettings are the directory parameters as a member or router
// applies them.
type DirectorySettings struct {
	Replication int
	Reprovide   time.Duration
	RecordTTL   time.Duration
}

// DefaultDirectorySettings is what a member or router runs on when the
// control plane publishes no directory parameters.
func DefaultDirectorySettings() DirectorySettings {
	return DirectorySettings{
		Replication: DefaultDHTReplication,
		Reprovide:   DefaultReprovideInterval,
		RecordTTL:   RecordTTL(DefaultReprovideInterval),
	}
}

// DirectorySettingsFrom reads the parameters a control plane published,
// with the default for each it left unset or unusable, so a member or
// router never runs with a zero.
func DirectorySettingsFrom(p *DirectoryParameters) DirectorySettings {
	s := DefaultDirectorySettings()
	if p == nil {
		return s
	}
	if r := int(p.GetReplication()); r >= 1 {
		s.Replication = r
	}
	if d := p.GetReprovideInterval().AsDuration(); p.GetReprovideInterval() != nil && d > 0 {
		s.Reprovide = d
		s.RecordTTL = RecordTTL(d)
	}
	if d := p.GetRecordTtl().AsDuration(); p.GetRecordTtl() != nil && d > s.Reprovide {
		s.RecordTTL = d
	}
	return s
}
