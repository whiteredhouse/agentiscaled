package caller

import "regexp"

// Service metadata keys under which a node says what runs one of its
// agents: the runtime (codex, claude, acp) and the node's agentiscaled
// version. The node writes them for itself, so they inform what callers and
// the owner's console show; nothing relies on them for access.
const (
	RuntimeMetadataKey     = "agentiscale.runtime"
	NodeVersionMetadataKey = "agentiscale.node_version"
)

var (
	runtimeWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	versionWord = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$`)
)

// ValidRuntime reports whether runtime can be an agent's runtime as callers
// see it: a lower-case word of up to 32 letters, digits and dashes.
func ValidRuntime(runtime string) bool { return runtimeWord.MatchString(runtime) }

// ValidNodeVersion reports whether version can be a node's version as
// callers see it: up to 64 letters, digits and . + _ -, such as 1.2.3,
// 1.2.3-rc.1 or dev.
func ValidNodeVersion(version string) bool { return versionWord.MatchString(version) }

// RuntimeFromMetadata is the runtime in an agent's service metadata, or ""
// when there is none or it is not a valid one.
func RuntimeFromMetadata(md map[string]string) string {
	if r := md[RuntimeMetadataKey]; ValidRuntime(r) {
		return r
	}
	return ""
}

// NodeVersionFromMetadata is the node version in an agent's service
// metadata, or "" when there is none or it is not a valid one.
func NodeVersionFromMetadata(md map[string]string) string {
	if v := md[NodeVersionMetadataKey]; ValidNodeVersion(v) {
		return v
	}
	return ""
}
