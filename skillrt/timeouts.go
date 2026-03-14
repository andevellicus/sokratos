package skillrt

import "time"

const (
	TimeoutSkillExec           = 30 * time.Second
	TimeoutSkillExecDelegation = 5 * time.Minute
	TimeoutSkillHTTP           = 15 * time.Second
	TimeoutSkillKV             = 5 * time.Second
	TimeoutDelegateCall        = 60 * time.Second
	TimeoutDelegateBatch       = 3 * time.Minute
)
