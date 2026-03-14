package grammar

import (
	"fmt"
	"strings"

	"sokratos/toolreg"
)

// BuildSubagentToolGrammar generates a GBNF grammar for a subagent's tool
// execution loop. The grammar constrains output to either a tool call (scoped
// to the provided schemas) or a final response.
//
// Output format:
//
//	{"action":"tool","name":"<tool>","arguments":{...}}
//	{"action":"respond","text":"<response>"}
func BuildSubagentToolGrammar(schemas []toolreg.ToolSchema) string {
	if len(schemas) == 0 {
		// No tools — grammar only allows respond.
		return `# Subagent grammar (no tools)
ws ::= [ \t\n\r]*
escaped-char ::= "\\" ["\\/bfnrt]
safe-char ::= [^"\\]
string-content ::= (safe-char | escaped-char)*

root ::= respond
respond ::= "{" ws "\"action\"" ws ":" ws "\"respond\"" ws "," ws "\"text\"" ws ":" ws "\"" string-content "\"" ws "}"
`
	}

	var b strings.Builder

	// Shared primitives.
	b.WriteString(`# Subagent tool grammar
ws ::= [ \t\n\r]*
escaped-char ::= "\\" ["\\/bfnrt]
safe-char ::= [^"\\]
string-content ::= (safe-char | escaped-char)*
string ::= "\"" string-content "\""
number ::= "-"? [0-9]+ ("." [0-9]+)?
boolean ::= "true" | "false"
value ::= string | number | boolean | array | object
array ::= "[" ws "]" | "[" ws value (ws "," ws value)* ws "]"
object ::= "{" ws "}" | "{" ws string ws ":" ws value (ws "," ws string ws ":" ws value)* ws "}"

`)

	// Root: tool-call | respond.
	b.WriteString("root ::= tool-call | respond\n")
	b.WriteString(`respond ::= "{" ws "\"action\"" ws ":" ws "\"respond\"" ws "," ws "\"text\"" ws ":" ws "\"" string-content "\"" ws "}"`)
	b.WriteString("\n\n")

	// Tool name alternatives.
	var toolNames []string
	for _, s := range schemas {
		toolNames = append(toolNames, fmt.Sprintf(`"%s"`, s.Name))
	}

	// Build per-tool argument rules.
	// Rule names must use hyphens only — underscores silently break grammar
	// enforcement in llama-server (grammar compiles but sampling is unconstrained).
	var argRuleNames []string
	for _, s := range schemas {
		ruleName := "args-" + strings.ReplaceAll(s.Name, "_", "-")
		argRuleNames = append(argRuleNames, ruleName)

		if len(s.Params) == 0 {
			fmt.Fprintf(&b, "%s ::= \"{\" ws \"}\"\n", ruleName)
		} else {
			fmt.Fprintf(&b, "%s ::= \"{\" ws ", ruleName)
			for i, p := range s.Params {
				if i > 0 {
					b.WriteString(` ws "," ws `)
				}
				typeRule := paramTypeToRule(p.Type)
				fmt.Fprintf(&b, `"\"" "%s" "\"" ws ":" ws %s`, p.Name, typeRule)
			}
			b.WriteString(" ws \"}\"\n")
		}
	}

	// tool-call rule: dispatch to the correct args rule based on tool name.
	// Each tool gets its own tool-call variant to pair name with correct args.
	var toolCallAlts []string
	for i, s := range schemas {
		altName := "tool-call-" + strings.ReplaceAll(s.Name, "_", "-")
		toolCallAlts = append(toolCallAlts, altName)
		fmt.Fprintf(&b, `%s ::= "{" ws "\"action\"" ws ":" ws "\"tool\"" ws "," ws "\"name\"" ws ":" ws "\"%s\"" ws "," ws "\"arguments\"" ws ":" ws %s ws "}"`,
			altName, s.Name, argRuleNames[i])
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "tool-call ::= %s\n", strings.Join(toolCallAlts, " | "))

	return b.String()
}
