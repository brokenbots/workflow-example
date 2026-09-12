package criteria

import pb "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"

// ForEachEntered is emitted when execution enters a step that is about to iterate.
type ForEachEntered = pb.ForEachEntered

// StepIterationStarted is emitted at the start of each iteration of a step loop (W10).
// Replaces ForEachIteration from W07.
type StepIterationStarted = pb.StepIterationStarted

// StepIterationCompleted is emitted when a step finishes all its iterations (W10).
// Replaces ForEachOutcome from W07.
type StepIterationCompleted = pb.StepIterationCompleted

// StepIterationItem is emitted when the engine is about to execute the step body
// for the next iteration item (W10). Replaces ForEachStep from W08.
type StepIterationItem = pb.StepIterationItem

// ScopeIterCursorSet is emitted when the loop-iteration cursor variable is written.
type ScopeIterCursorSet = pb.ScopeIterCursorSet
