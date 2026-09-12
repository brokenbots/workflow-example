package criteria

import pb "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"

// WaitEntered is emitted when execution reaches a wait node.
type WaitEntered = pb.WaitEntered

// WaitResumed is emitted when a wait node is released by a signal.
type WaitResumed = pb.WaitResumed

// ApprovalRequested is emitted when an approval node requires a decision.
type ApprovalRequested = pb.ApprovalRequested

// ApprovalDecision is emitted when an approval decision is recorded.
type ApprovalDecision = pb.ApprovalDecision
