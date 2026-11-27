package caller

import (
	"bytes"
	"encoding/json"
	"fmt"
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
	// Drained says that the agent has no task left: none running and none
	// waiting for an answer. The node sets it from what the agent is doing
	// each time it publishes the drain, and never saves it in a drain file. Without
	// it the agent is still draining, also when its node is too old to say.
	Drained bool `json:"drained,omitempty"`
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

// maxDrainByBytes bounds a drain's By: a word such as DrainSetOnNode.
const maxDrainByBytes = 32

// maxDrainMetadataBytes is what Agent Mesh takes in one service metadata
// value.
const maxDrainMetadataBytes = 512

// validDrainBy reports whether by is a word a drain's By may be: lower-case
// letters, digits and dashes.
func validDrainBy(by string) bool {
	return len(by) <= maxDrainByBytes && !strings.ContainsFunc(by, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-'
	})
}

// Valid is the drain without the fields it may not carry: a reason or By
// that would not fit, or a time JSON cannot write, which only a hand-edited drain file or another node
// can hold. The mark itself always stays.
func (d Drain) Valid() Drain {
	if DrainReasonError(d.Reason) != "" {
		d.Reason = ""
	}
	if !validDrainBy(d.By) {
		d.By = ""
	}
	// A time can read and still not write, such as one 24 hours ahead of
	// UTC; it would leave the drain, and every answer that carries it,
	// unwritable.
	if _, err := d.Since.MarshalJSON(); err != nil {
		d.Since = time.Time{}
	}
	return d
}

// Metadata is the drain's service metadata value, which always fits in
// one: it leaves out any field the drain may not carry, and keeps only
// the time and Drained should the rest still not fit.
func (d Drain) Metadata() string {
	v := encodeDrain(d.Valid())
	if len(v) > maxDrainMetadataBytes {
		v = encodeDrain(Drain{Since: d.Since, Drained: d.Drained})
	}
	return v
}

func encodeDrain(d Drain) string {
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
	d = d.Valid()
	return &d
}

// Word is the drain's state: "drained" once the agent has no task left,
// "draining" before.
func (d Drain) Word() string {
	if d.Drained {
		return "drained"
	}
	return "draining"
}

// Text is how a drain reads after an agent's name: "is draining: reason".
func (d Drain) Text() string {
	if d.Reason == "" {
		return "is " + d.Word()
	}
	return "is " + d.Word() + ": " + d.Reason
}

// State is how the agent lists show a drain in their STATE column: its
// word, how long ago it began as of now, and its reason, such as
// "drained (2h5m): reboot".
func (d Drain) State(now time.Time) string {
	text := d.Word()
	if !d.Since.IsZero() {
		text += SinceText(&d.Since, now)
	}
	if d.Reason != "" {
		text += ": " + d.Reason
	}
	return text
}

// SinceText is how long ago t was as of now, as the agent lists show it
// after a state: " (42s)", " (5m)", " (18h29m)"; "" without a time.
func SinceText(t *time.Time, now time.Time) string {
	if t == nil {
		return ""
	}
	d := max(now.Sub(*t), 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf(" (%ds)", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf(" (%dm)", int(d.Minutes()))
	}
	return fmt.Sprintf(" (%dh%dm)", int(d.Hours()), int(d.Minutes())%60)
}
