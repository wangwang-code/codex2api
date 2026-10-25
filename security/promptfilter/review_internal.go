package promptfilter

import (
	"context"
	"strings"
	"sync/atomic"
)

// ReviewInternalReason is written to usage_logs.internal_reason for audit-model
// calls that never leave the process. Keeping it stable lets the usage table
// separate reviewer traffic from real users after the loopback is gone.
const ReviewInternalReason = "prompt_audit_review"

// InternalExecutor runs one chat-completions request without leaving the
// process. It is deliberately narrow: the reviewer builds the same JSON body it
// would have POSTed, and receives the same (status, body) the wire would have
// returned, so response decoding stays identical for both transports.
//
// Handles() is what keeps this honest: an executor must only claim base URLs
// that really point at the process it lives in. A review endpoint aimed at a
// different host must keep going over HTTP.
type InternalExecutor interface {
	Handles(baseURL string) bool
	ExecuteChatCompletion(ctx context.Context, body []byte, reason string, reasoningEffort string) (status int, respBody []byte)
}

type internalExecutorHolder struct {
	exec InternalExecutor
}

var internalExecutor atomic.Pointer[internalExecutorHolder]

// RegisterInternalExecutor installs the in-process transport. Called once at
// startup by main; passing nil unregisters it (used by tests to isolate).
func RegisterInternalExecutor(exec InternalExecutor) {
	if exec == nil {
		internalExecutor.Store(nil)
		return
	}
	internalExecutor.Store(&internalExecutorHolder{exec: exec})
}

// internalExecutorFor returns the executor only when it claims this base URL.
// A nil result means "use HTTP", which is the safe default: a misconfigured or
// absent registration must never silently change where review traffic goes.
func internalExecutorFor(baseURL string) InternalExecutor {
	holder := internalExecutor.Load()
	if holder == nil || holder.exec == nil {
		return nil
	}
	if !holder.exec.Handles(strings.TrimSpace(baseURL)) {
		return nil
	}
	return holder.exec
}
