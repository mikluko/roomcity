// Package roomtool holds the host's work-tool answers whose wording the chat
// renderer reads.
package roomtool

import "strings"

// NoTaskAnswer is the host's answer when a worker has no task to claim.
const NoTaskAnswer = "There is no task for you; this session is ending."

const (
	dispatchPrefix = "Handed to a worker as task "
	claimPrefix    = "Task "
	claimSeparator = ":\n"
)

// DispatchAnswer tells the agent the id of the task it handed to a worker.
func DispatchAnswer(taskID string) string {
	return dispatchPrefix + taskID + "."
}

// DispatchedTask reads the task id from a dispatch answer.
func DispatchedTask(answer string) string {
	rest, ok := strings.CutPrefix(answer, dispatchPrefix)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(rest, ".")
}

// ClaimAnswer gives a worker the id and text of the task it claimed.
func ClaimAnswer(taskID, task string) string {
	return claimPrefix + taskID + claimSeparator + task
}

// ClaimedTask reads the task id and text from a claim answer.
func ClaimedTask(answer string) (id, task string, ok bool) {
	rest, ok := strings.CutPrefix(answer, claimPrefix)
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, claimSeparator)
}
