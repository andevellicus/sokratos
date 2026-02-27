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

//go:embed distill_conversation.txt
var DistillConversation string

//go:embed extract_tasks.txt
var ExtractTasks string

//go:embed bootstrap.txt
var Bootstrap string

//go:embed subagent.txt
var Subagent string

//go:embed transition.txt
var Transition string
