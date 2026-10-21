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
	"slices"
	"strings"
)

// ValidateServiceFormat ensures the service string follows the explicit URI format.
func ValidateServiceFormat(svc string) error {
	if svc == "*" {
		return nil
	}

	matches := rfc3986URIRegex.FindStringSubmatch(svc)
	if len(matches) < 6 {
		return fmt.Errorf("invalid service format %q: must follow explicit URI format (e.g., mcp://name)", svc)
	}

	scheme := matches[2]
	hasAuthority := matches[3] != ""
	authority := matches[4]

	if scheme == "" || !hasAuthority {
		return fmt.Errorf("invalid service format %q: must follow explicit URI format (e.g., mcp://name)", svc)
	}

	if matches[6] != "" {
		return fmt.Errorf("invalid service format %q: query parameters are not allowed in service URIs", svc)
	}
	if matches[8] != "" {
		return fmt.Errorf("invalid service format %q: fragments are not allowed in service URIs", svc)
	}

	if strings.Contains(authority, "@") {
		return fmt.Errorf("invalid service format %q: userinfo is not allowed in service URIs", svc)
	}

	typ, val := ParseServiceTarget(svc)
	if typ == "" {
		return fmt.Errorf("invalid service format %q: type cannot be empty", svc)
	}
	if val == "" {
		return fmt.Errorf("invalid service format %q: value cannot be empty", svc)
	}

	if val == "*" {
		return nil
	}

	// Split val into host and path (if any)
	parts := strings.SplitN(val, "/", 2)
	host := parts[0]

	if !dnsNameRegex.MatchString(host) {
		return fmt.Errorf("invalid service format %q: %q is not a valid DNS name", svc, host)
	}

	return nil
}

// ValidateTargetFormat ensures the target string follows the explicit fact:value format.
func ValidateTargetFormat(target string) error {
	if target == "*" {
		return nil
	}
	parts := strings.SplitN(target, ":", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid target format %q: must be fact:value (e.g., group:backend)", target)
	}
	fact, val := parts[0], parts[1]
	if fact == "" {
		return fmt.Errorf("invalid target format %q: fact cannot be empty", target)
	}
	if val == "" {
		return fmt.Errorf("invalid target format %q: value cannot be empty", target)
	}
	if err := validateBindingMemberCharset(val); err != nil {
		return fmt.Errorf("invalid target format %q: %w", target, err)
	}
	// "*" as the fact matches every target_fact (granted_target_all_facts).
	if fact != "*" && !slices.Contains(TargetFactNames(), fact) {
		return fmt.Errorf("invalid target %q: %q is not a target fact, so nothing would ever match it (want %s or \"*\")", target, fact, strings.Join(TargetFactNames(), ", "))
	}
	return nil
}
