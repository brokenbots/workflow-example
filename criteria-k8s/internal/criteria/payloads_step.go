//nolint:revive // Proto-generated LogStream_* constant names are wire-compatibility shims and cannot be renamed.
package criteria

import pb "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"

// StepEntered is emitted when execution enters a step node.
type StepEntered = pb.StepEntered

// StepOutcome is emitted when a step completes (success or failure).
type StepOutcome = pb.StepOutcome

// StepTransition is emitted when the FSM transitions between step states.
type StepTransition = pb.StepTransition

// StepLog carries a log line produced during step execution.
type StepLog = pb.StepLog

// LogStream identifies which output stream a StepLog chunk came from.
type LogStream = pb.LogStream

// LogStream enum constants (original generated names preserved for drop-in compatibility).
const (
	LogStream_LOG_STREAM_UNSPECIFIED LogStream = pb.LogStream_LOG_STREAM_UNSPECIFIED
	LogStream_LOG_STREAM_STDOUT      LogStream = pb.LogStream_LOG_STREAM_STDOUT
	LogStream_LOG_STREAM_STDERR      LogStream = pb.LogStream_LOG_STREAM_STDERR
	LogStream_LOG_STREAM_AGENT       LogStream = pb.LogStream_LOG_STREAM_AGENT
)

// StepResumed is emitted when a paused step resumes execution.
type StepResumed = pb.StepResumed

// StepOutputCaptured is emitted when a step's output is captured.
type StepOutputCaptured = pb.StepOutputCaptured
