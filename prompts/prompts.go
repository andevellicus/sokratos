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

//go:embed reflection.txt
var Reflection string

//go:embed distill_conversation.txt
var DistillConversation string

//go:embed bootstrap.txt
var Bootstrap string

//go:embed transition.txt
var Transition string

//go:embed plan_task.txt
var PlanTask string

//go:embed replan_task.txt
var ReplanTask string

//go:embed heartbeat_mode.txt
var HeartbeatMode string

//go:embed routine_mode.txt
var RoutineMode string

//go:embed objective_inference.txt
var ObjectiveInference string

//go:embed dispatch_triage.txt
var DispatchTriage string

//go:embed session_create_skill.txt
var SessionCreateSkill string

//go:embed session_send_email.txt
var SessionSendEmail string

//go:embed session_reason.txt
var SessionReason string
