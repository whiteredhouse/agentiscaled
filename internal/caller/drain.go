package caller

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// DrainMetadataKey is the service metadata key under which a node says that
// one of its agents is draining: it finishes the tasks it has, answers to
// its questions included, and takes no new ones. Its value is a Drain in
// JSON.
const DrainMetadataKey = "agentiscale.mark.drain"

// MaxDrainReasonBytes bounds a drain's reason, so that its mark always fits
// in the agent's service metadata.
const MaxDrainReasonBytes = 200

// DrainSetOnNode is a Drain's By when the agent's owner set it on the
// agent's own machine (agentiscaled agent drain).
const DrainSetOnNode = "node"

// Drain is an owner's mark on an agent that takes no new tasks.
type Drain struct {
	// Reason is the owner's note, which callers see with the refusal.
	Reason string `json:"reason,omitempty"`
	// Since is when the drain began.
	Since time.Time `json:"since,omitzero"`
	// By says where the drain was set.
	By string `json:"by,omitempty"`
}

// DrainReasonError says what is wrong with a drain's reason, or "" when
// nothing is.
func DrainReasonError(reason string) string {
	switch {
	case len(reason) > MaxDrainReasonBytes:
		return "is too long: keep it to " + strconv.Itoa(MaxDrainReasonBytes) + " bytes"
	case !utf8.ValidString(reason):
		return "is not valid UTF-8"
	case strings.ContainsFunc(reason, unicode.IsControl):
		return "has a line break or another control character"
	}
	return ""
}

// Metadata is the drain's service metadata value. It leaves out a reason
// that would not fit, which only a hand-edited drain file can hold.
func (d Drain) Metadata() string {
	if DrainReasonError(d.Reason) != "" {
		d.Reason = ""
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(d)
	return strings.TrimSuffix(b.String(), "\n")
}

// DrainFromMetadata reads an agent's drain from its service metadata, or
// nil when it has none. The mark only keeps callers from sending new tasks,
// so any value under the key counts as a drain; a reason or time that
// cannot be read is left out.
func DrainFromMetadata(md map[string]string) *Drain {
	v, ok := md[DrainMetadataKey]
	if !ok {
		return nil
	}
	var d Drain
	if json.Unmarshal([]byte(v), &d) != nil {
		return &Drain{}
	}
	if DrainReasonError(d.Reason) != "" {
		d.Reason = ""
	}
	return &d
}

// Text is how a drain reads after an agent's name: "is draining: reason".
func (d Drain) Text() string {
	if d.Reason == "" {
		return "is draining"
	}
	return "is draining: " + d.Reason
}
