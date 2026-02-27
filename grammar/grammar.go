package grammar

import (
	"fmt"
	"strings"

	"sokratos/tools"
)

// BuildToolGrammar generates a GBNF grammar that constrains LLM output to
// valid tool-call JSON objects. Each registered tool gets its own rule that
// enforces exact parameter names and types. The root rule is a union of all
// tool alternatives.
func BuildToolGrammar(schemas []tools.ToolSchema) string {
	if len(schemas) == 0 {
		return ""
	}

	var b strings.Builder

	// Shared primitives.
	b.WriteString(`# Shared primitives
ws ::= [ \t\n\r]*
string ::= "\"" ([^"\\] | "\\" .)* "\""
number ::= "-"? [0-9]+ ("." [0-9]+)?
boolean ::= "true" | "false"
value ::= string | number | boolean | array | object
array ::= "[" ws "]" | "[" ws value (ws "," ws value)* ws "]"
object ::= "{" ws "}" | "{" ws string ws ":" ws value (ws "," ws string ws ":" ws value)* ws "}"

`)

	// Build root as union of all tool rules.
	var toolRuleNames []string
	for _, s := range schemas {
		ruleName := "tool-" + s.Name
		toolRuleNames = append(toolRuleNames, ruleName)
	}
	fmt.Fprintf(&b, "root ::= %s\n\n", strings.Join(toolRuleNames, " | "))

	// Per-tool rules.
	for _, s := range schemas {
		ruleName := "tool-" + s.Name
		if len(s.Params) == 0 {
			// Tool with no arguments: {"name":"<name>","arguments":{}}
			fmt.Fprintf(&b, `%s ::= "{" ws "\"name\"" ws ":" ws "\"%s\"" ws "," ws "\"arguments\"" ws ":" ws "{" ws "}" ws "}"`, ruleName, s.Name)
			b.WriteString("\n")
		} else {
			// Tool with arguments: {"name":"<name>","arguments":{<params>}}
			fmt.Fprintf(&b, `%s ::= "{" ws "\"name\"" ws ":" ws "\"%s\"" ws "," ws "\"arguments\"" ws ":" ws "{" ws `, ruleName, s.Name)

			for i, p := range s.Params {
				if i > 0 {
					b.WriteString(` ws "," ws `)
				}
				typeRule := paramTypeToRule(p.Type)
				fmt.Fprintf(&b, `"\"" "%s" "\"" ws ":" ws %s`, p.Name, typeRule)
			}

			b.WriteString(` ws "}" ws "}"`)
			b.WriteString("\n")
		}
	}

	return b.String()
}

// BuildTriageGrammar generates a GBNF grammar that constrains LLM output to
// a valid triage JSON object with salience_score, summary, category, tags, and save.
func BuildTriageGrammar() string {
	return `# Triage output grammar
ws ::= [ \t\n\r]*
string ::= "\"" ([^"\\] | "\\" .)* "\""
number ::= [0-9]+ ("." [0-9]+)?
boolean ::= "true" | "false"
string-array ::= "[" ws "]" | "[" ws string (ws "," ws string)* ws "]"

root ::= "{" ws "\"salience_score\"" ws ":" ws number ws "," ws "\"summary\"" ws ":" ws string ws "," ws "\"category\"" ws ":" ws string ws "," ws "\"tags\"" ws ":" ws string-array ws "," ws "\"save\"" ws ":" ws boolean ws "," ws "\"paradigm_shift\"" ws ":" ws boolean ws "}"
`
}

// paramTypeToRule maps a schema type string to the corresponding GBNF rule name.
func paramTypeToRule(t string) string {
	switch t {
	case "string":
		return "string"
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "array":
		return "array"
	default:
		return "value"
	}
}
