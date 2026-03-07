package tools

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dop251/goja"

	"sokratos/clients"
)

// skillSetupUtils registers the simple JS globals: btoa, atob, sleep, env,
// hash_sha256, hash_hmac_sha256.
func skillSetupUtils(vm *goja.Runtime, ctx context.Context) {
	// btoa / atob (base64)
	vm.Set("btoa", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("btoa requires 1 argument"))
		}
		return vm.ToValue(base64.StdEncoding.EncodeToString([]byte(call.Arguments[0].String())))
	})
	vm.Set("atob", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("atob requires 1 argument"))
		}
		decoded, err := base64.StdEncoding.DecodeString(call.Arguments[0].String())
		if err != nil {
			panic(vm.NewTypeError("atob: invalid base64: " + err.Error()))
		}
		return vm.ToValue(string(decoded))
	})

	// sleep(ms)
	vm.Set("sleep", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			return goja.Undefined()
		}
		ms := call.Arguments[0].ToInteger()
		if ms <= 0 {
			return goja.Undefined()
		}
		dur := time.Duration(ms) * time.Millisecond
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := time.Until(deadline); dur > remaining {
				dur = remaining
			}
		}
		if dur > 5*time.Second {
			dur = 5 * time.Second
		}
		time.Sleep(dur)
		if ctx.Err() != nil {
			panic(vm.NewTypeError("context cancelled during sleep"))
		}
		return goja.Undefined()
	})

	// env(key)
	vm.Set("env", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("env requires 1 argument"))
		}
		val := os.Getenv("SKILL_" + call.Arguments[0].String())
		if val == "" {
			return goja.Undefined()
		}
		return vm.ToValue(val)
	})

	// hash_sha256 / hash_hmac_sha256
	vm.Set("hash_sha256", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 1 {
			panic(vm.NewTypeError("hash_sha256 requires 1 argument"))
		}
		h := sha256.Sum256([]byte(call.Arguments[0].String()))
		return vm.ToValue(hex.EncodeToString(h[:]))
	})
	vm.Set("hash_hmac_sha256", func(call goja.FunctionCall) goja.Value {
		if len(call.Arguments) < 2 {
			panic(vm.NewTypeError("hash_hmac_sha256 requires 2 arguments: key, message"))
		}
		mac := hmac.New(sha256.New, []byte(call.Arguments[0].String()))
		mac.Write([]byte(call.Arguments[1].String()))
		return vm.ToValue(hex.EncodeToString(mac.Sum(nil)))
	})
}

// skillSetupConsole registers console.log/warn/error. Appends output to logBuf.
func skillSetupConsole(vm *goja.Runtime, logBuf *[]string) {
	consoleObj := vm.NewObject()
	for _, level := range []string{"log", "warn", "error"} {
		lvl := level
		consoleObj.Set(lvl, func(call goja.FunctionCall) goja.Value {
			parts := make([]string, len(call.Arguments))
			for i, arg := range call.Arguments {
				parts[i] = arg.String()
			}
			*logBuf = append(*logBuf, fmt.Sprintf("[%s] %s", strings.ToUpper(lvl), strings.Join(parts, " ")))
			return goja.Undefined()
		})
	}
	vm.Set("console", consoleObj)
}

// skillSetupKV registers kv_get, kv_set, kv_delete backed by PostgreSQL.
// If pool is nil, registers stubs that panic with an unavailable message.
func skillSetupKV(vm *goja.Runtime, deps SkillDeps, ctx context.Context, name string) {
	if deps.Pool != nil {
		pool := deps.Pool
		vm.Set("kv_get", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("kv_get requires 1 argument: key"))
			}
			key := call.Arguments[0].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			var val string
			err := pool.QueryRow(kvCtx,
				"SELECT value FROM skill_kv WHERE skill_name=$1 AND key=$2", name, key).Scan(&val)
			if err != nil {
				return goja.Undefined()
			}
			return vm.ToValue(val)
		})
		vm.Set("kv_set", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("kv_set requires 2 arguments: key, value"))
			}
			key := call.Arguments[0].String()
			value := call.Arguments[1].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			_, err := pool.Exec(kvCtx,
				`INSERT INTO skill_kv (skill_name, key, value, updated_at) VALUES ($1, $2, $3, now())
				 ON CONFLICT (skill_name, key) DO UPDATE SET value=$3, updated_at=now()`,
				name, key, value)
			if err != nil {
				panic(vm.NewTypeError("kv_set failed: " + err.Error()))
			}
			return goja.Undefined()
		})
		vm.Set("kv_delete", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("kv_delete requires 1 argument: key"))
			}
			key := call.Arguments[0].String()
			kvCtx, cancel := context.WithTimeout(ctx, TimeoutSkillKV)
			defer cancel()
			_, err := pool.Exec(kvCtx,
				"DELETE FROM skill_kv WHERE skill_name=$1 AND key=$2", name, key)
			if err != nil {
				panic(vm.NewTypeError("kv_delete failed: " + err.Error()))
			}
			return goja.Undefined()
		})
	} else {
		kvUnavailable := func(call goja.FunctionCall) goja.Value {
			panic(vm.NewTypeError("kv store unavailable: no database connection"))
		}
		vm.Set("kv_get", kvUnavailable)
		vm.Set("kv_set", kvUnavailable)
		vm.Set("kv_delete", kvUnavailable)
	}
}

// skillSetupDelegation registers call_tool, delegate, delegate_batch.
// If deps lack Registry/SC/DC, registers stubs that panic with an unavailable message.
func skillSetupDelegation(vm *goja.Runtime, deps SkillDeps, ctx context.Context, name string) {
	if deps.Registry != nil && deps.SC != nil && deps.DC != nil {
		vm.Set("call_tool", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 2 {
				panic(vm.NewTypeError("call_tool requires 2 arguments: name, args"))
			}
			toolName := call.Arguments[0].String()
			if toolName == name {
				panic(vm.NewTypeError("call_tool: cannot call self (" + name + ")"))
			}
			toolArgs := call.Arguments[1].Export()
			result, err := deps.Registry.Call(ctx, toolName, toolArgs)
			if err != nil {
				panic(vm.NewTypeError("call_tool(" + toolName + "): " + err.Error()))
			}
			return vm.ToValue(result)
		})

		vm.Set("delegate", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("delegate requires at least 1 argument: directive"))
			}
			directive := call.Arguments[0].String()
			if len(call.Arguments) > 1 && !goja.IsUndefined(call.Arguments[1]) {
				ctxData := call.Arguments[1].String()
				if len(ctxData) > maxDelegateContextLen {
					ctxData = ctxData[:maxDelegateContextLen] + "\n... (truncated)"
				}
				directive = directive + "\n\n## Context\n" + ctxData
			}
			toolExec := NewScopedToolExec(deps.Registry, deps.DC)
			dCtx, dCancel := context.WithTimeout(ctx, TimeoutDelegateCall)
			defer dCancel()
			result, err := clients.SubagentSupervisor(dCtx, deps.SC, deps.DC.Grammar(), delegateSystemPrompt, directive, toolExec, 10, nil)
			if err != nil {
				panic(vm.NewTypeError("delegate failed: " + err.Error()))
			}
			return vm.ToValue(result)
		})

		vm.Set("delegate_batch", func(call goja.FunctionCall) goja.Value {
			if len(call.Arguments) < 1 {
				panic(vm.NewTypeError("delegate_batch requires 1 argument: tasks array"))
			}
			tasksRaw := call.Arguments[0].Export()
			tasksSlice, ok := tasksRaw.([]interface{})
			if !ok {
				panic(vm.NewTypeError("delegate_batch: tasks must be an array"))
			}
			if len(tasksSlice) == 0 {
				return vm.ToValue([]interface{}{})
			}

			type taskInput struct {
				directive string
				context   string
			}
			inputs := make([]taskInput, len(tasksSlice))
			for i, raw := range tasksSlice {
				obj, ok := raw.(map[string]interface{})
				if !ok {
					panic(vm.NewTypeError(fmt.Sprintf("delegate_batch: task[%d] must be an object with {directive, context}", i)))
				}
				d, _ := obj["directive"].(string)
				if d == "" {
					panic(vm.NewTypeError(fmt.Sprintf("delegate_batch: task[%d].directive is required", i)))
				}
				c, _ := obj["context"].(string)
				inputs[i] = taskInput{directive: d, context: c}
			}

			batchCtx, batchCancel := context.WithTimeout(ctx, TimeoutDelegateBatch)
			defer batchCancel()

			type batchResult struct {
				Result string
				Error  string
			}
			results := make([]batchResult, len(inputs))
			total := len(inputs)

			// Cap concurrent supervisors to available subagent slots.
			used, total := deps.SC.SlotsInUse()
			avail := total - used
			if avail <= 0 {
				avail = 1 // always allow at least one
			}
			concSem := make(chan struct{}, avail)

			var completed atomic.Int32
			var wg sync.WaitGroup
			for i, inp := range inputs {
				wg.Add(1)
				go func(idx int, inp taskInput) {
					defer wg.Done()
					concSem <- struct{}{} // wait for a concurrency slot
					defer func() { <-concSem }()
					directive := inp.directive
					if inp.context != "" {
						ctxData := inp.context
						if len(ctxData) > maxDelegateContextLen {
							ctxData = ctxData[:maxDelegateContextLen] + "\n... (truncated)"
						}
						directive = directive + "\n\n## Context\n" + ctxData
					}
					toolExec := NewScopedToolExec(deps.Registry, deps.DC)
					result, err := clients.SubagentSupervisor(batchCtx, deps.SC, deps.DC.Grammar(), delegateSystemPrompt, directive, toolExec, 10, nil)
					if err != nil {
						results[idx] = batchResult{Error: err.Error()}
					} else {
						results[idx] = batchResult{Result: result}
					}
					n := completed.Add(1)
					ReportProgress(ctx, fmt.Sprintf("Completed %d/%d tasks...", n, total))
				}(i, inp)
			}
			wg.Wait()

			jsResults := make([]interface{}, len(results))
			for i, r := range results {
				jsResults[i] = map[string]interface{}{
					"result": r.Result,
					"error":  r.Error,
				}
			}
			return vm.ToValue(jsResults)
		})
	} else {
		delegationUnavailable := func(fnName string) func(goja.FunctionCall) goja.Value {
			return func(call goja.FunctionCall) goja.Value {
				panic(vm.NewTypeError(fnName + " unavailable: delegation dependencies not configured"))
			}
		}
		vm.Set("call_tool", delegationUnavailable("call_tool"))
		vm.Set("delegate", delegationUnavailable("delegate"))
		vm.Set("delegate_batch", delegationUnavailable("delegate_batch"))
	}
}
