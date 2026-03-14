package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sokratos/engine"
	"sokratos/llm"
	"sokratos/logger"
	"sokratos/orchestrate"
	"sokratos/prompts"
	"sokratos/textutil"
)

// brainSessionPrompts maps task types to their Brain session system prompts.
var brainSessionPrompts = map[string]string{
	"create_skill": prompts.SessionCreateSkill,
	"send_email":   prompts.SessionSendEmail,
}

// brainSessionAcks maps tool names to acknowledgement messages sent when
// a background Brain job is spawned.
var brainSessionAcks = map[string]string{
	"create_skill": "I'll work on creating that skill in the background. You can keep chatting — I'll let you know when it's ready or if I have questions.",
	"send_email":   "I'll draft that email in the background. You can keep chatting — I'll send it your way for review shortly.",
}

// mandatedBrainTools maps tools that MUST run as background Brain jobs to their
// task_type (used for session prompt selection). The orchestrator is intercepted
// at the loop level if it tries to call these directly.
var mandatedBrainTools = map[string]string{
	"create_skill": "create_skill",
	"update_skill": "create_skill", // same session prompt as create_skill
}

// ---------------------------------------------------------------------------
// Background Brain jobs — async sessions for complex tool calls.
// ---------------------------------------------------------------------------

// buildJobContext generates a system prompt injection block describing active
// background jobs so the orchestrator can route user messages to them.
func buildJobContext(jobs []*engine.BackgroundJob) string {
	if len(jobs) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("[Active Background Jobs]\n")
	sb.WriteString("Background Brain jobs are running. If the user's message relates to one, call reply_to_job or cancel_job.\n\n")
	for _, j := range jobs {
		isActive, lastQ, _ := j.Snapshot()
		status := "working"
		if !isActive && lastQ != "" {
			status = "waiting for input"
		}
		fmt.Fprintf(&sb, "- Job %s: tool=%s, goal=%q, status=%s", j.ID, j.Tool, textutil.Truncate(j.UserGoal, 100), status)
		if lastQ != "" {
			fmt.Fprintf(&sb, ", last_question=%q", textutil.Truncate(lastQ, 150))
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// runBackgroundJob runs a Brain session in a goroutine, executing multi-round
// tool calls with the ability to ask the user questions and receive replies.
func runBackgroundJob(mc messageContext, job *engine.BackgroundJob) {
	defer mc.svc.StateMgr.RemoveJob(job.ID)

	// Select session prompt by TaskType, falling back to general reasoning prompt.
	// Injected as a user message prefix — NOT as a system message — because
	// the orchestrator prepends its own system message and Qwen3.5's Jinja
	// template raises "System message must be at the beginning" if a second
	// system message appears mid-conversation.
	sessionPrompt := brainSessionPrompts[job.TaskType]
	if sessionPrompt == "" {
		sessionPrompt = prompts.SessionReason
	}

	messages := []llm.Message{
		{Role: "user", Content: sessionPrompt + "\n\n" + job.UserGoal},
	}

	const maxRounds = 20

	for range maxRounds {
		ctx, cancel := context.WithCancel(context.Background())
		job.SetActive(true, cancel)

		choice := mc.router.AcquireOrFallback(ctx, true, engine.PriorityUser) // preferBrain

		// Wrap toolExec to detect when the triggering tool succeeds.
		// Block recursive deep_think calls — the Brain IS the deep thinker.
		sessionToolExec := func(execCtx context.Context, raw json.RawMessage) (string, error) {
			var call struct {
				Name string `json:"name"`
			}
			json.Unmarshal(raw, &call)
			if call.Name == "deep_think" {
				return "You ARE the deep thinker. Call the tools directly (create_skill, search_web, etc.) instead of deep_think.", nil
			}
			result, err := mc.confirmExec(execCtx, raw)
			if err == nil && call.Name == job.Tool && !orchestrate.IsToolSoftError(result) {
				job.SetToolSucceeded(true)
			}
			return result, err
		}

		reply, newMsgs, qErr := llm.QueryOrchestrator(ctx, choice.Client, choice.Model,
			messages[len(messages)-1].Content, sessionToolExec, nil, &llm.QueryOrchestratorOpts{
				ToolAgent:      mc.lb.ToolAgent,
				History:        messages[:len(messages)-1],
				EnableThinking: true, // Brain should reason deeply
				// No MandatedBrainTools — Brain should execute tools directly.
			})

		choice.Release()
		// Check ctx.Err() BEFORE cancel() — cancel() would set it unconditionally,
		// masking the real error. A non-nil ctx.Err() here means external cancellation
		// (e.g. user called cancel_job).
		ctxCancelled := ctx.Err() != nil
		cancel()
		job.SetActive(false, nil)

		if qErr != nil {
			if ctxCancelled {
				mc.platform.Send(context.Background(), job.ChannelID, "Background job cancelled.", "")
			} else {
				logger.Log.Warnf("[job:%s] error: %v", job.ID, qErr)
				mc.platform.Send(context.Background(), job.ChannelID, "Background job error: "+qErr.Error(), "")
			}
			return
		}

		messages = append(messages, newMsgs...)
		_, _, toolSucceeded := job.Snapshot()

		if toolSucceeded {
			// Tool succeeded — send the Brain's final reply and record in conversation.
			mc.platform.Send(context.Background(), job.ChannelID, reply, "")
			mc.svc.StateMgr.AppendMessage(llm.Message{
				Role:    "assistant",
				Content: fmt.Sprintf("[Background %s completed: %s]", job.Tool, textutil.Truncate(reply, 200)),
			})
			return
		}

		// Brain has a question or produced output — send to user, park goroutine.
		mc.platform.Send(context.Background(), job.ChannelID, reply, "")
		job.SetLastQuestion(reply)

		// Park — slot released, goroutine blocks waiting for user input.
		input, inputOK := <-job.InputCh
		if !inputOK {
			mc.platform.Send(context.Background(), job.ChannelID, "Background job cancelled.", "")
			return
		}
		job.SetLastQuestion("")
		messages = append(messages, llm.Message{Role: "user", Content: input})
	}

	mc.platform.Send(context.Background(), job.ChannelID, "Background job reached maximum rounds without completing.", "")
}
