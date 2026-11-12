package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

// A task's progress reaches its caller as short lines, each the agent's
// latest step ("Running go", "Editing main.go", "Working on step 2 of 5"),
// sent as the message of the task's working status. The caller sees the
// latest one when it follows the task (get_task).
//
// A step goes to another node and stays in the task's history, so it is a
// fixed summary of the activity: a command's program, a file's base name, a
// tool's name, a todo item's position. Never a command line, path, URL,
// search, todo text or what the agent says, any of which can carry a
// credential or other private value of the agent's machine.

// maxProgressLine bounds one progress line, in characters.
const maxProgressLine = 200

// maxRuntimeLine bounds one line of a runtime's event stream. A longer line,
// such as a large tool output, is skipped, never kept whole.
const maxRuntimeLine = 16 << 20

// lineWriter is a runtime's stdout: it calls line with each line written,
// without the newline, and keeps nothing else. The line is only valid
// during the call.
type lineWriter struct {
	line    func([]byte)
	partial []byte
	skip    bool // in a line over maxRuntimeLine
}

func (w *lineWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !w.skip && len(w.partial)+len(chunk) > maxRuntimeLine {
			w.partial, w.skip = nil, true
		}
		if !w.skip {
			w.partial = append(w.partial, chunk...)
		}
		if i < 0 {
			break
		}
		if !w.skip {
			w.line(w.partial)
		}
		w.partial, w.skip = w.partial[:0], false
		p = p[i+1:]
	}
	return n, nil
}

// Close passes on a last line that has no newline.
func (w *lineWriter) Close() {
	if !w.skip && len(w.partial) > 0 {
		w.line(w.partial)
	}
	w.partial, w.skip = nil, false
}

// progressLine is s on one line, at most maxProgressLine characters.
func progressLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxProgressLine {
		return s
	}
	r := []rune(s)
	return string(r[:maxProgressLine-1]) + "…"
}

// plainName is a name that is safe to show as it is: a program, a file's
// base name or a tool, with no spaces, quotes, slashes or other syntax a
// value could hide in.
var plainName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

// fileName is the base name of path, or "a file" when that is not a plain
// name.
func fileName(path string) string {
	if name := filepath.Base(strings.TrimSpace(path)); plainName.MatchString(name) {
		return name
	}
	return "a file"
}

// commandStep names the program a command line runs, looking through
// variable assignments and a shell's -c, and never shows its arguments.
func commandStep(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	if len(fields) >= 3 && slices.Contains([]string{"bash", "sh", "zsh"}, filepath.Base(fields[0])) && slices.Contains([]string{"-c", "-lc"}, fields[1]) {
		return commandStep(strings.Trim(strings.Join(fields[2:], " "), `'"`))
	}
	if len(fields) > 0 {
		if prog := filepath.Base(fields[0]); plainName.MatchString(prog) {
			return "Running " + prog
		}
	}
	return "Running a command"
}

// todoStep is the position of the first unfinished item of a todo list.
func todoStep(done []bool) string {
	for i, d := range done {
		if !d {
			return fmt.Sprintf("Working on step %d of %d", i+1, len(done))
		}
	}
	return ""
}

// claudeProgress is the progress line of one event of `claude -p
// --output-format stream-json`, or "" for an event that is not a step: a
// tool the agent calls or its todo list.
func claudeProgress(event []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(event, &ev) != nil || ev.Type != "assistant" {
		return ""
	}
	var line string
	for _, c := range ev.Message.Content {
		if c.Type == "tool_use" {
			if s := claudeToolStep(c.Name, c.Input); s != "" {
				line = s
			}
		}
	}
	return progressLine(line)
}

// claudeToolStep says what a Claude Code tool call does.
func claudeToolStep(name string, input json.RawMessage) string {
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Todos        []struct {
			Status string `json:"status"`
		} `json:"todos"`
	}
	_ = json.Unmarshal(input, &in)
	switch name {
	case "", "StructuredOutput":
		// StructuredOutput is how the turn's answer (--json-schema) ends
		// the run, not a step.
		return ""
	case "Bash":
		return commandStep(in.Command)
	case "Read":
		return "Reading " + fileName(in.FilePath)
	case "Edit", "MultiEdit":
		return "Editing " + fileName(in.FilePath)
	case "Write":
		return "Writing " + fileName(in.FilePath)
	case "NotebookEdit":
		return "Editing " + fileName(in.NotebookPath)
	case "Grep", "Glob":
		return "Searching files"
	case "WebFetch":
		return "Fetching a web page"
	case "WebSearch":
		return "Searching the web"
	case "Task", "Agent":
		return "Running a subagent"
	case "TodoWrite":
		var done []bool
		for _, t := range in.Todos {
			done = append(done, t.Status == "completed")
		}
		return todoStep(done)
	}
	if plainName.MatchString(name) {
		return "Using " + name
	}
	return "Using a tool"
}

// codexProgress is the progress line of one event of `codex exec --json`,
// or "" for an event that is not a step: a command, an applied file change,
// a tool call, a search or its todo list.
func codexProgress(event []byte) string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Command string `json:"command"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Changes []struct {
				Path string `json:"path"`
			} `json:"changes"`
			Items []struct {
				Completed bool `json:"completed"`
			} `json:"items"`
		} `json:"item"`
	}
	if json.Unmarshal(event, &ev) != nil || !strings.HasPrefix(ev.Type, "item.") {
		return ""
	}
	it := ev.Item
	var line string
	switch {
	case it.Type == "command_execution" && ev.Type == "item.started":
		line = commandStep(it.Command)
	case it.Type == "mcp_tool_call" && ev.Type == "item.started":
		line = "Using a tool"
		if name := strings.Trim(it.Server+"."+it.Tool, "."); plainName.MatchString(name) {
			line = "Using " + name
		}
	case it.Type == "web_search" && ev.Type == "item.started":
		line = "Searching the web"
	case it.Type == "file_change" && ev.Type == "item.completed" && it.Status == "completed":
		// A patch that failed changed nothing.
		var names []string
		for _, c := range it.Changes {
			names = append(names, fileName(c.Path))
		}
		if len(names) > 3 {
			names = append(names[:3], fmt.Sprintf("%d more", len(it.Changes)-3))
		}
		if len(names) > 0 {
			line = "Changed " + strings.Join(names, ", ")
		}
	case it.Type == "todo_list":
		var done []bool
		for _, t := range it.Items {
			done = append(done, t.Completed)
		}
		line = todoStep(done)
	}
	return progressLine(line)
}
