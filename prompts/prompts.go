package prompts

import _ "embed"

//go:embed system.txt
var System string

//go:embed tools.txt
var Tools string

//go:embed deep_thinker.txt
var DeepThinker string

//go:embed consolidation.txt
var Consolidation string

//go:embed email_triage.txt
var EmailTriage string

//go:embed conversation_triage.txt
var ConversationTriage string

//go:embed calendar_triage.txt
var CalendarTriage string

//go:embed reflection.txt
var Reflection string
