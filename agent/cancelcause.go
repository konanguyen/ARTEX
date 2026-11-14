package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused the task",
		"The user paused the task via task control (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run was cancelled; running intents return to frontier (open) to be claimed and executed from the beginning upon resumption.")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestration agent paused the task",
		"The orchestration agent invoked the pause_task tool to pause this task. This Planner/Worker run was cancelled; running intents return to frontier (open) to be re-executed upon resumption.")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted",
		"The task is being deleted (DELETE /api/tasks/{id}); deletion barrier cancelled running Planner, Worker, and Main Agent. Results of this run will no longer be used.")
	AbortPausedOnReload = cause("paused_on_reload", "Backend restored paused state on reload",
		"Backend restored the task's paused state from the database during startup. This run was cancelled; normally no agent is running during recovery.")
	AbortGoalMet = cause("goal_met", "Planner determined task goal was met",
		"The planner determined that the task goal was achieved and marked the task as done, then cancelled remaining running workers; these intents will be marked as stopped, not failed.")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task timeout drain grace expired",
		"The task reached timeout and waited for running workers to finish gracefully, but the 90-second drain grace period was exceeded, so a hard cancellation was performed; intents are marked as exhausted, while recorded facts and assets are retained.")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent",
		"The planner invoked kill_work to terminate this intent, usually indicating deviation or no further value; the intent will be marked as stopped and will not be re-claimed.")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused this worker intent",
		"The user paused the running worker. This invocation was cancelled and the intent transitioned to paused; recorded intents, facts, findings, and activity records are retained, and will re-execute from the beginning upon resumption.")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted this worker intent",
		"The user deleted the running worker. This invocation was cancelled; after the worker exits the write region, the server handles deletion per selected mode — soft delete marks deleted and retains all outputs, hard delete cascades removal of this intent and orphan downstream nodes.")
	AbortWorkClaimLost = cause("work_claim_lost", "Another Worker took this intent over",
		"This Worker's claim on the intent expired (no heartbeat reached the database within the claim TTL) and another Worker took the intent over, so its heartbeat was rejected. This run was cancelled and its result is discarded; the intent now belongs to the new claimer")
	AbortWorkFinished = cause("work_finished", "Worker finished normally",
		"The worker finished normally and the engine released its context resources in detachWork. This is not an interruption; if appearing in an abort notice, it indicates a race between cancellation and cleanup.")
	AbortPausedRaceGuard = cause("paused_race_guard", "Refusing run while task is paused",
		"While the task is paused, the engine refuses to issue a new execution context to prevent races between claim and pause; claimed intents return to frontier.")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "User stopped this conversation turn",
		"The user clicked stop, intentionally aborting this turn of the Main Agent or conversation agent. Existing activity records are retained, and the next message can be sent.")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task paused and aborted chat",
		"When the user paused the task, running main agent conversations were cancelled synchronously. Existing activity records are retained; this turn will not automatically replay upon task resumption.")
	AbortChatTurnFinished = cause("chat_turn_finished", "Turn finished normally",
		"This turn finished normally and the server is releasing context resources. This is not an interruption; if appearing in an abort notice, it indicates a race between cancellation and cleanup.")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is shutting down",
		"Backend process received SIGINT or SIGTERM and is restarting, updating, or shutting down. All running agents will be cancelled; orphan running intents will reset to open upon restart and re-execute.")
	AbortRunHardTimeout = cause("run_hard_timeout", "Hard timeout backstop triggered",
		"Single run exceeded soft wall-clock budget and extra grace period, indicating a model request or tool call did not return for an extended period, preventing normal turn wrap-up. Please check the last hanging tool call before interruption.")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context reached deadline",
			"Upstream context reached deadline, but setter did not attach a named cause via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Canceler did not attach a named cause",
			"Upstream context was canceled, but canceler did not attach a named cause via context.WithCancelCause; please register cause in agent/cancelcause.go", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
