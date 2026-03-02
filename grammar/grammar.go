package grammar

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
