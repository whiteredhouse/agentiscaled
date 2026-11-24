// Package cli provides command-line parsing and display conventions shared by the node and caller commands.
package cli

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
)

// ParseInterleaved accepts flags before or after positionals and honors --.
func ParseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		arg := args[0]
		if arg == "--" {
			return append(pos, args[1:]...), nil
		}
		if len(arg) < 2 || arg[0] != '-' {
			pos = append(pos, arg)
			args = args[1:]
			continue
		}
		// One flag at a time, with its value when that is the next
		// argument, so that a value such as "--" is never taken for the
		// end of the flags.
		n := 1
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if f := fs.Lookup(name); f != nil && !hasValue && len(args) > 1 {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
				n = 2
			}
		}
		if err := fs.Parse(args[:n]); err != nil {
			return nil, err
		}
		args = args[n:]
	}
	return pos, nil
}

// IsTerminal reports whether f is a character device.
func IsTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// HomeRelative displays paths inside the user's home relative to ~.
func HomeRelative(path string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return filepath.Join("~", rel)
		}
	}
	return path
}

// ShellQuote formats s as a single shell argument.
func ShellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:@+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
