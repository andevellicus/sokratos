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

// BuildDispatchGrammar generates a GBNF grammar that constrains subagent triage
// output to either an escalation (dispatch:false) or a dispatch decision with
// tool name and arguments (dispatch:true). Static grammar — tool name validation
// happens after parse.
func BuildDispatchGrammar() string {
	return `# Dispatch triage grammar
ws ::= [ \t\n\r]*
string ::= "\"" ([^"\\] | "\\" .)* "\""
number ::= [0-9]+ ("." [0-9]+)?
boolean ::= "true" | "false"
null ::= "null"
value ::= string | number | boolean | null | object | array
array ::= "[" ws "]" | "[" ws value (ws "," ws value)* ws "]"
object ::= "{" ws "}" | "{" ws string ws ":" ws value (ws "," ws string ws ":" ws value)* ws "}"

root ::= escalate | dispatch | multi
escalate ::= "{" ws "\"dispatch\"" ws ":" ws "false" ws "," ws "\"ack\"" ws ":" ws string ws "}"
dispatch ::= "{" ws "\"dispatch\"" ws ":" ws "true" ws "," ws "\"tool\"" ws ":" ws string ws "," ws "\"args\"" ws ":" ws object ws "," ws "\"ack\"" ws ":" ws string ws "}"
multi ::= "{" ws "\"dispatch\"" ws ":" ws "true" ws "," ws "\"multi\"" ws ":" ws "true" ws "," ws "\"directive\"" ws ":" ws string ws "," ws "\"ack\"" ws ":" ws string ws "}"
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
