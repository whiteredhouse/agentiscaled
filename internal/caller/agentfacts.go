package caller

import (
	"encoding/json"
	"regexp"
)

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

// ModelMetadataKey is the service metadata key under which a node says
// which model one of its agents asks its runtime for: the model its
// definition sets, or "" when the agent sets none and the runtime picks
// its own default (from the owner's settings, say). A node too old to say
// leaves the key out. Like the runtime, the node writes it for itself, so
// it informs display only.
const ModelMetadataKey = "agentiscale.model"

// modelWord is the form of a model as callers see it: what Claude Code and
// Codex take as a model's name or alias, including provider IDs such as
// us.anthropic.claude-…-v1:0 or a [1m] suffix.
var modelWord = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/@\[\]-]{0,127}$`)

// ValidModel reports whether model can be an agent's model as callers see
// it: up to 128 letters, digits and ._:/@[]- starting with a letter or
// digit. "" is valid too: the runtime's default.
func ValidModel(model string) bool { return model == "" || modelWord.MatchString(model) }

// ModelFromMetadata is the model in an agent's service metadata. ok is false
// when the node does not say (one older than it) or what it says is not a
// valid model; model is "" when the agent runs its runtime's default.
func ModelFromMetadata(md map[string]string) (model string, ok bool) {
	m, ok := md[ModelMetadataKey]
	if !ok || !ValidModel(m) {
		return "", false
	}
	return m, true
}

// ShownModel is a model as tables show it: the model, "default" for the
// runtime's own, and "-" when the node does not say.
func ShownModel(model *string) string {
	switch {
	case model == nil:
		return "-"
	case *model == "":
		return "default"
	}
	return *model
}

// SkillsMetadataKey is the service metadata key under which a node lists
// the skills one of its agents loads, by name, for the owner's console and
// discovery. Its value is a Skills in JSON. Like the runtime, the node
// writes it for itself; the agent's card holds the skills in full.
const SkillsMetadataKey = "agentiscale.skills"

// Skills is what an agent's service metadata says of its skills: the names
// that fit in it, in the card's order, and how many the card lists. Total
// is more than len(Names) when some names did not fit.
type Skills struct {
	Names []string `json:"names"`
	Total int      `json:"total"`
}

// skillWord is the form of a skill name on a card (the Agent Skills
// format): up to 64 lower-case letters, digits and single dashes.
var skillWord = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// ValidSkillName reports whether name can be a skill's name as callers see
// it.
func ValidSkillName(name string) bool { return len(name) <= 64 && skillWord.MatchString(name) }

// SkillsMetadata is the metadata value that lists names, the skills of an
// agent, in at most room bytes: the first names that fit, and all of them
// counted in Total. Names that are not valid skill names are left out. It
// is "" when not even the count fits, or when there are no skills.
func SkillsMetadata(names []string, room int) string {
	valid := make([]string, 0, len(names))
	for _, n := range names {
		if ValidSkillName(n) {
			valid = append(valid, n)
		}
	}
	if len(valid) == 0 {
		return ""
	}
	s := Skills{Names: []string{}, Total: len(valid)}
	if len(encodeSkills(s)) > room {
		return ""
	}
	// Each name adds itself, its quotes and a comma; names need no
	// escaping.
	size := len(encodeSkills(s))
	for _, n := range valid {
		add := len(n) + 2
		if len(s.Names) > 0 {
			add++
		}
		if size+add > room {
			break
		}
		s.Names = append(s.Names, n)
		size += add
	}
	return encodeSkills(s)
}

func encodeSkills(s Skills) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// SkillsFromMetadata reads an agent's skills from its service metadata, or
// nil when it lists none or the value cannot be read. Names that are not
// valid skill names are left out, and Total is never less than the names
// listed.
func SkillsFromMetadata(md map[string]string) *Skills {
	v, ok := md[SkillsMetadataKey]
	if !ok {
		return nil
	}
	var s Skills
	if json.Unmarshal([]byte(v), &s) != nil {
		return nil
	}
	names := make([]string, 0, len(s.Names))
	for _, n := range s.Names {
		if ValidSkillName(n) {
			names = append(names, n)
		}
	}
	s.Names = names
	if s.Total < len(names) {
		s.Total = len(names)
	}
	if s.Total == 0 {
		return nil
	}
	return &s
}
