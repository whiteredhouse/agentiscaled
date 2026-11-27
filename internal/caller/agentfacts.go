package caller

import "regexp"

// RuntimeMetadataKey is the service metadata key under which a node says
// what runs one of its agents (codex, claude, acp). The node writes it for
// itself, so it informs what callers and the owner's console show; nothing
// relies on it for access.
const RuntimeMetadataKey = "agentiscale.runtime"

var runtimeWord = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidRuntime reports whether runtime can be an agent's runtime as callers
// see it: a lower-case word of up to 32 letters, digits and dashes.
func ValidRuntime(runtime string) bool { return runtimeWord.MatchString(runtime) }

// RuntimeFromMetadata is the runtime in an agent's service metadata, or ""
// when there is none or it is not a valid one.
func RuntimeFromMetadata(md map[string]string) string {
	if r := md[RuntimeMetadataKey]; ValidRuntime(r) {
		return r
	}
	return ""
}
