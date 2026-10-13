package executor

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
)

// HostCommandResult is the structured outcome for one host from an ad-hoc command.
type HostCommandResult struct {
	Status  string `json:"status"`
	RC      int    `json:"rc"`
	Changed bool   `json:"changed"`
	Stdout  string `json:"stdout,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
}

// ParseAnsibleAdhocOutput aggregates raw output lines from an ad-hoc Ansible run
// into a per-host result dictionary. It handles both command/shell module output
// (with `>>`) and JSON module output (with `=>`).
func ParseAnsibleAdhocOutput(lines []string) map[string]HostCommandResult {
	results := make(map[string]HostCommandResult)
	if len(lines) == 0 {
		return results
	}

	type hostBlock struct {
		host    string
		status  string
		isJSON  bool
		rc      int
		changed bool
		lines   []string
	}

	var blocks []hostBlock
	var current *hostBlock

	for _, line := range lines {
		raw := strings.TrimSpace(line)
		if strings.HasPrefix(raw, "[system] ") || strings.HasPrefix(raw, "[system]") ||
			strings.HasPrefix(raw, "succeeded (exit ") || strings.HasPrefix(raw, "failed (exit ") ||
			strings.HasPrefix(raw, "[WARNING]:") || strings.HasPrefix(raw, "[DEPRECATION WARNING]:") {
			continue
		}
		for _, prefix := range []string{"[info] ", "[error] "} {
			if strings.HasPrefix(raw, prefix) {
				raw = strings.TrimPrefix(raw, prefix)
				break
			}
		}

		// Check for host header
		host, rest, ok := strings.Cut(raw, " | ")
		isHeader := false
		if ok && host != "" && strings.IndexFunc(host, unicode.IsSpace) < 0 {
			switch {
			case strings.Contains(rest, " >>"):
				// Pattern: <host> | <STATUS> | rc=<rc> >>
				parts := strings.Split(rest, " | ")
				status := strings.TrimSpace(parts[0])
				rc := 0
				if len(parts) > 1 {
					rcStr := strings.TrimSpace(parts[1])
					rcStr = strings.TrimSuffix(rcStr, " >>")
					if strings.HasPrefix(rcStr, "rc=") {
						if n, err := strconv.Atoi(strings.TrimPrefix(rcStr, "rc=")); err == nil {
							rc = n
						}
					}
				}
				if current != nil {
					blocks = append(blocks, *current)
				}
				current = &hostBlock{
					host:    host,
					status:  status,
					isJSON:  false,
					rc:      rc,
					changed: status == "CHANGED",
				}
				isHeader = true

			case strings.Contains(rest, " =>"):
				// Pattern: <host> | <STATUS> => [optional JSON start]
				statusPart, jsonStart, _ := strings.Cut(rest, " =>")
				status := strings.TrimSpace(statusPart)
				if current != nil {
					blocks = append(blocks, *current)
				}
				current = &hostBlock{
					host:    host,
					status:  status,
					isJSON:  true,
					rc:      0,
					changed: status == "CHANGED",
				}
				if strings.HasPrefix(status, "FAIL") || strings.HasPrefix(status, "UNREACHABLE") {
					current.rc = 1
				}
				isHeader = true
				if trimmedJSON := strings.TrimSpace(jsonStart); trimmedJSON != "" {
					current.lines = append(current.lines, trimmedJSON)
				}
			}
		}

		if !isHeader && current != nil {
			current.lines = append(current.lines, raw)
		}
	}

	if current != nil {
		blocks = append(blocks, *current)
	}

	for _, b := range blocks {
		res := HostCommandResult{
			Status:  b.status,
			RC:      b.rc,
			Changed: b.changed,
		}

		if b.isJSON {
			jsonBody := strings.TrimSpace(strings.Join(b.lines, "\n"))
			var data map[string]any
			if err := json.Unmarshal([]byte(jsonBody), &data); err == nil {
				if ch, ok := data["changed"].(bool); ok {
					res.Changed = ch
				}
				if rcVal, ok := data["rc"].(float64); ok {
					res.RC = int(rcVal)
				}
				if stdout, ok := data["stdout"].(string); ok {
					res.Stdout = stdout
				}
				if stderr, ok := data["stderr"].(string); ok {
					res.Stderr = stderr
				}
				if msg, ok := data["msg"].(string); ok {
					if res.Stderr == "" && (res.RC != 0 || strings.Contains(b.status, "FAIL") || strings.Contains(b.status, "UNREACHABLE")) {
						res.Stderr = msg
					} else if res.Stdout == "" {
						res.Stdout = msg
					}
				}
				if res.Stdout == "" && res.Stderr == "" {
					// Fallback to formatted json
					res.Stdout = jsonBody
				}
			} else {
				res.Stdout = jsonBody
			}
		} else {
			output := strings.TrimSpace(strings.Join(b.lines, "\n"))
			if b.rc == 0 {
				res.Stdout = output
			} else {
				res.Stderr = output
			}
		}

		results[b.host] = res
	}

	return results
}
