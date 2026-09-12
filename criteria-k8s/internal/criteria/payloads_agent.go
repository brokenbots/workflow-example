package criteria

import pb "github.com/brokenbots/workflow-example/criteria-k8s/internal/criteria/pb/criteria/v1"

// CriteriaHeartbeat is emitted periodically by the criteria agent to signal liveness.
type CriteriaHeartbeat = pb.CriteriaHeartbeat

// CriteriaDisconnected is emitted when the criteria agent disconnects from the orchestrator.
type CriteriaDisconnected = pb.CriteriaDisconnected

// WatchReady is emitted by the orchestrator to signal that a WatchRun stream
// is live and ready to receive events.
type WatchReady = pb.WatchReady
