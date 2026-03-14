// Package tools re-exports types and functions from the toolreg, skillrt, and
// worktracker packages so that the ~25 tool implementation files in this package
// can continue using unqualified names without import changes.
//
// External importers (main, grammar/, engine/) should import the leaf packages
// directly instead of going through these aliases.
package tools

import (
	"encoding/json"

	"sokratos/skillrt"
	"sokratos/toolreg"
	"sokratos/worktracker"
)

// --- toolreg type aliases ---

type ToolError = toolreg.ToolError
type ToolCall = toolreg.ToolCall
type ToolFunc = toolreg.ToolFunc
type ParamSchema = toolreg.ParamSchema
type ToolSchema = toolreg.ToolSchema
type Registry = toolreg.Registry
type DelegateConfig = toolreg.DelegateConfig
type ApprovalCache = toolreg.ApprovalCache
type ProgressFunc = toolreg.ProgressFunc

// --- toolreg function/var aliases ---

var Errorf = toolreg.Errorf
var NewRegistry = toolreg.NewRegistry
var NewScopedToolExec = toolreg.NewScopedToolExec
var NewDelegateConfig = toolreg.NewDelegateConfig
var NewApprovalCache = toolreg.NewApprovalCache
var WithProgress = toolreg.WithProgress
var ReportProgress = toolreg.ReportProgress

// ParseArgs wraps the generic toolreg.ParseArgs (generic funcs can't be aliased via var).
func ParseArgs[T any](args json.RawMessage) (T, error) {
	return toolreg.ParseArgs[T](args)
}

// --- skillrt type aliases ---

type SkillManifest = skillrt.SkillManifest
type Skill = skillrt.Skill
type SkillDeps = skillrt.SkillDeps
type GrammarRebuildFunc = skillrt.GrammarRebuildFunc

// --- skillrt function aliases ---

var LoadSkills = skillrt.LoadSkills
var RegisterSkill = skillrt.RegisterSkill
var SyncSkills = skillrt.SyncSkills
var ExecuteSkill = skillrt.ExecuteSkill
var NewCreateSkill = skillrt.NewCreateSkill
var NewManageSkills = skillrt.NewManageSkills
var NewUpdateSkill = skillrt.NewUpdateSkill
var ValidateTypeScriptSource = skillrt.ValidateTypeScriptSource
var ValidateSkillSource = skillrt.ValidateSkillSource

// --- worktracker type aliases ---

type WorkTracker = worktracker.WorkTracker
type PlanExecDeps = worktracker.PlanExecDeps
type ObjectiveTaskResult = worktracker.ObjectiveTaskResult
type Scratchpad = worktracker.Scratchpad

// --- worktracker function aliases ---

var LaunchBackgroundPlan = worktracker.LaunchBackgroundPlan
var NewPlanAndExecute = worktracker.NewPlanAndExecute
var NewCheckBackgroundTask = worktracker.NewCheckBackgroundTask
