package caller

import (
	"slices"
	"strings"
	"testing"
)

// The value lists the first names that fit in its room and counts them
// all, so the console can say how many it does not name.
func TestSkillsMetadataFitsItsRoom(t *testing.T) {
	names := []string{"deploy", "release", "review", "Not A Name", strings.Repeat("a", 65)}
	all := SkillsMetadata(names, 512)
	if all != `{"names":["deploy","release","review"],"total":3}` {
		t.Fatalf("value = %s", all)
	}
	for room := len(all); room >= 0; room-- {
		v := SkillsMetadata(names, room)
		if len(v) > room {
			t.Fatalf("room %d: %d bytes: %s", room, len(v), v)
		}
		if v == "" {
			if room >= len(`{"names":[],"total":3}`) {
				t.Fatalf("room %d: nothing, though the count fits", room)
			}
			continue
		}
		s := SkillsFromMetadata(map[string]string{SkillsMetadataKey: v})
		if s == nil || s.Total != 3 || !slices.Equal(s.Names, []string{"deploy", "release", "review"}[:len(s.Names)]) {
			t.Fatalf("room %d: %+v", room, s)
		}
	}
	if v := SkillsMetadata([]string{"Not A Name"}, 512); v != "" {
		t.Fatalf("no valid names: %q", v)
	}
	if v := SkillsMetadata(nil, 512); v != "" {
		t.Fatalf("no names: %q", v)
	}
}

// What another node says is read with care: names that are not skill names
// are left out, and the count never says fewer than the names listed.
func TestSkillsFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  *Skills
	}{
		{`{"names":["a","b"],"total":5}`, &Skills{Names: []string{"a", "b"}, Total: 5}},
		{`{"names":["a","B c","\u001b[2J"],"total":1}`, &Skills{Names: []string{"a"}, Total: 1}},
		{`{"names":["a","b"],"total":0}`, &Skills{Names: []string{"a", "b"}, Total: 2}},
		{`{"names":[],"total":0}`, nil},
		{`["a"]`, nil},
		{`not json`, nil},
	} {
		got := SkillsFromMetadata(map[string]string{SkillsMetadataKey: tc.value})
		if (got == nil) != (tc.want == nil) || got != nil && (got.Total != tc.want.Total || !slices.Equal(got.Names, tc.want.Names)) {
			t.Errorf("%s: %+v, want %+v", tc.value, got, tc.want)
		}
	}
	if SkillsFromMetadata(map[string]string{}) != nil {
		t.Error("no key: skills")
	}
}

// A model reads back as the node wrote it; "" is the runtime's default,
// and a missing key or a value that is not a model says nothing.
func TestModelFromMetadata(t *testing.T) {
	for _, tc := range []struct {
		md    map[string]string
		model string
		ok    bool
	}{
		{map[string]string{ModelMetadataKey: "sonnet"}, "sonnet", true},
		{map[string]string{ModelMetadataKey: "claude-opus-4-1[1m]"}, "claude-opus-4-1[1m]", true},
		{map[string]string{ModelMetadataKey: "arn:aws:bedrock:eu-west-2:123456789012:inference-profile/eu.anthropic.claude"}, "arn:aws:bedrock:eu-west-2:123456789012:inference-profile/eu.anthropic.claude", true},
		{map[string]string{ModelMetadataKey: ""}, "", true},
		{map[string]string{ModelMetadataKey: strings.Repeat("m", 128)}, strings.Repeat("m", 128), true},
		{map[string]string{ModelMetadataKey: strings.Repeat("m", 129)}, "", false},
		{map[string]string{ModelMetadataKey: "gpt 5"}, "", false},
		{map[string]string{ModelMetadataKey: "-rf"}, "", false},
		{map[string]string{ModelMetadataKey: "opus\x1b[2J"}, "", false},
		{map[string]string{}, "", false},
		{nil, "", false},
	} {
		if model, ok := ModelFromMetadata(tc.md); model != tc.model || ok != tc.ok {
			t.Errorf("%q: %q %v, want %q %v", tc.md[ModelMetadataKey], model, ok, tc.model, tc.ok)
		}
	}
	sonnet, none := "sonnet", ""
	for model, want := range map[*string]string{&sonnet: "sonnet", &none: "default", nil: "-"} {
		if got := ShownModel(model); got != want {
			t.Errorf("ShownModel = %q, want %q", got, want)
		}
	}
}
