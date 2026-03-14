package worktracker

import "time"

const (
	TimeoutPlanDecomposition = 60 * time.Second
	TimeoutPlanStepExecution = 90 * time.Second
	TimeoutPlanForeground    = 5 * time.Minute
	TimeoutPlanBackground    = 15 * time.Minute
	TimeoutPlanProgressDB    = 5 * time.Second
)
