package codex

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Suppress only shell scripts consisting entirely of identifiable om calls.
// Ambiguous input is captured: a mention of om in an edit, search, or another
// command's arguments is evidence, not a recursive memory operation.
func memoryCommand(event map[string]any, executable, store, session string) bool {
	if field(event, "tool_name") != "Bash" {
		return false
	}
	input, ok := event["tool_input"].(map[string]any)
	if !ok {
		return false
	}
	command := field(input, "command")
	if command == "" {
		command = field(input, "cmd")
	}
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return false
	}
	safe, calls := true, 0
	syntax.Walk(file, func(node syntax.Node) bool {
		if !safe {
			return false
		}
		switch node := node.(type) {
		case *syntax.CallExpr:
			calls++
			if len(node.Args) == 0 {
				safe = false
				break
			}
			name, literal := staticWord(node.Args[0].Parts)
			safe = literal && (name == executable || name == "om" && hasScope(node.Args[1:], store, session))
		case *syntax.Redirect:
			// Input heredocs/files are normal apply/capture operations. Preserve
			// evidence when the script also writes an unrelated output file.
			safe = node.Op == syntax.Hdoc || node.Op == syntax.DashHdoc || node.Op == syntax.RdrIn
		case *syntax.FuncDecl, *syntax.ForClause, *syntax.WhileClause, *syntax.IfClause, *syntax.CaseClause:
			safe = false
		}
		return safe
	})
	return safe && calls > 0
}

func hasScope(words []*syntax.Word, store, session string) bool {
	values := map[string]string{}
	for i := 0; i+1 < len(words); i++ {
		flag, ok := staticWord(words[i].Parts)
		if !ok || flag != "--store" && flag != "--session" {
			continue
		}
		if value, ok := staticWord(words[i+1].Parts); ok {
			values[flag] = value
		}
		i++
	}
	return values["--store"] == store && values["--session"] == session
}

func staticWord(parts []syntax.WordPart) (string, bool) {
	var text strings.Builder
	for _, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if strings.Contains(part.Value, "\\") {
				return "", false
			}
			text.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return "", false
			}
			text.WriteString(part.Value)
		case *syntax.DblQuoted:
			value, ok := staticWord(part.Parts)
			if !ok || part.Dollar {
				return "", false
			}
			text.WriteString(value)
		default:
			return "", false
		}
	}
	return text.String(), true
}
