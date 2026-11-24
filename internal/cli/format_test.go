package cli

import (
	"flag"
	"slices"
	"testing"
)

func TestParseInterleaved(t *testing.T) {
	for _, tc := range []struct {
		args []string
		pos  []string
		peer string
		js   bool
	}{
		{[]string{"a", "b"}, []string{"a", "b"}, "", false},
		{[]string{"-peer", "p", "a", "b"}, []string{"a", "b"}, "p", false},
		{[]string{"a", "-json", "b", "-peer", "p"}, []string{"a", "b"}, "p", true},
		{[]string{"a", "--", "-json"}, []string{"a", "-json"}, "", false},
		{[]string{"-json", "--", "-a", "-b"}, []string{"-a", "-b"}, "", true},
		{[]string{"a", "-"}, []string{"a", "-"}, "", false},
		{[]string{"-peer", "--", "a", "b"}, []string{"a", "b"}, "--", false},
		{[]string{"a", "-peer=--", "--", "-b"}, []string{"a", "-b"}, "--", false},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		p := fs.String("peer", "", "")
		j := fs.Bool("json", false, "")
		pos, err := ParseInterleaved(fs, tc.args)
		if err != nil || !slices.Equal(pos, tc.pos) || *p != tc.peer || *j != tc.js {
			t.Errorf("%q: %q peer=%q json=%t %v", tc.args, pos, *p, *j, err)
		}
	}
}
