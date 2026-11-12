package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// A task's progress reaches its caller as short lines, each the agent's
// latest step ("Bash: go test ./...", "Edit main.go"), sent as the message
// of the task's working status. The caller sees the latest one when it
// follows the task (get_task).

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
		if i < 0 {
			if !w.skip {
				w.partial = append(w.partial, p...)
				if len(w.partial) > maxRuntimeLine {
					w.partial, w.skip = nil, true
				}
			}
			break
		}
		if !w.skip {
			w.line(append(w.partial, p[:i]...))
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

// firstLine is the first non-empty line of s.
func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// claudeProgress is the progress line of one event of `claude -p
// --output-format stream-json`, or "" for an event that is not a step:
// a tool the agent calls, its todo list, or what it says between tools.
func claudeProgress(event []byte) string {
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
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
		switch c.Type {
		case "tool_use":
			if s := claudeToolStep(c.Name, c.Input); s != "" {
				line = s
			}
		case "text":
			// The structured answer that ends the turn is not a step.
			if t := firstLine(c.Text); t != "" && !strings.HasPrefix(t, "{") {
				line = t
			}
		}
	}
	return progressLine(line)
}

// claudeToolStep says what a Claude Code tool call does.
func claudeToolStep(name string, input json.RawMessage) string {
	if name == "" {
		return ""
	}
	if name == "TodoWrite" {
		var in struct {
			Todos []struct {
				Content    string `json:"content"`
				Status     string `json:"status"`
				ActiveForm string `json:"activeForm"`
			} `json:"todos"`
		}
		if json.Unmarshal(input, &in) == nil {
			for _, t := range in.Todos {
				if step := firstNonEmpty(t.ActiveForm, t.Content); t.Status == "in_progress" && step != "" {
					return "Working on: " + step
				}
			}
		}
		return ""
	}
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, key := range []string{"command", "file_path", "notebook_path", "pattern", "url", "query", "description"} {
		if v, ok := in[key].(string); ok && strings.TrimSpace(v) != "" {
			return name + ": " + v
		}
	}
	return name
}

// codexProgress is the progress line of one event of `codex exec --json`,
// or "" for an event that is not a step: a command, a file change, a tool
// call, a search, its todo list, or what it says between them.
func codexProgress(event []byte) string {
	var ev struct {
		Type string `json:"type"`
		Item struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Command string `json:"command"`
			Server  string `json:"server"`
			Tool    string `json:"tool"`
			Query   string `json:"query"`
			Changes []struct {
				Path string `json:"path"`
				Kind string `json:"kind"`
			} `json:"changes"`
			Items []struct {
				Text      string `json:"text"`
				Completed bool   `json:"completed"`
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
		line = "Running: " + it.Command
	case it.Type == "mcp_tool_call" && ev.Type == "item.started":
		line = "Tool: " + strings.Trim(it.Server+"."+it.Tool, ".")
	case it.Type == "web_search" && ev.Type == "item.started":
		line = "Searching: " + it.Query
	case it.Type == "file_change" && ev.Type == "item.completed":
		var paths []string
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		if len(paths) > 0 {
			line = "Changed " + strings.Join(paths, ", ")
		}
	case it.Type == "todo_list":
		for _, t := range it.Items {
			if !t.Completed && strings.TrimSpace(t.Text) != "" {
				line = "Working on: " + t.Text
				break
			}
		}
	case it.Type == "agent_message" && ev.Type == "item.completed":
		// The structured answer that ends the turn is not a step.
		if t := firstLine(it.Text); !strings.HasPrefix(t, "{") {
			line = t
		}
	}
	return progressLine(line)
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
