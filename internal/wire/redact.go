package wire

import "encoding/json"

// HasSensitiveValue reports whether a plan change actually carries a secret: a
// leaf marked true in the *_sensitive tree whose matching before/after value is
// set. It is the ONE classifier every face uses (web, TUI, API).
//
// It is stricter than "the tree marks anything" on purpose. A provider may mark
// a schema attribute sensitive whether or not it is used — hashicorp/local marks
// `sensitive_content` sensitive on every local_file — and tofu carries that true
// into the *_sensitive tree even when the attribute is unset. Keying a
// whole-resource redaction on the tree alone then hides a plaintext value behind
// [redacted · secret]. Requiring the value to be present keeps a genuine secret
// redacted and lets an untouched sensitive field through.
func HasSensitiveValue(sensitive json.RawMessage, value any) bool {
	if len(sensitive) == 0 {
		return false
	}
	var tree any
	if err := json.Unmarshal(sensitive, &tree); err != nil {
		return false
	}
	return sensitiveLeafSet(tree, value)
}

// sensitiveLeafSet walks a *_sensitive tree beside the value it mirrors: a
// `true` leaf counts only when the value at that path is present (non-null). A
// whole-tree `true` (a sensitive object) counts when the value itself is set.
func sensitiveLeafSet(tree, value any) bool {
	switch node := tree.(type) {
	case bool:
		return node && value != nil
	case map[string]any:
		m, _ := value.(map[string]any)
		for k, v := range node {
			var vv any
			if m != nil {
				vv = m[k]
			}
			if sensitiveLeafSet(v, vv) {
				return true
			}
		}
		return false
	case []any:
		s, _ := value.([]any)
		for i, v := range node {
			var vv any
			if i < len(s) {
				vv = s[i]
			}
			if sensitiveLeafSet(v, vv) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
