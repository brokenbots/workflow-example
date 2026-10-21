package controller

// Terminal archive gate (KB-314, castle #308 leg 2): every terminal
// CriteriaRun is archived into castle's RunArchiveService — with the run's
// identity, verdict and lifecycle summary — BEFORE the run's child Jobs are
// reaped and BEFORE the finalizer is removed. A failed archive stamps the
// TerminalArchivePending condition with a fresh status write and returns an
// error, so the pass requeues (controller-runtime's rate-limited backoff)
// with cleanup aborted: an unarchived terminal delete is data loss (#309
// retention deletes castle run history it has an archive record for), so
// the failure is never silently skipped. The gate is idempotent on the CR:
// a stamped Status.ArchiveID skips re-archiving, and castle itself is
// idempotent per archive key, so backfill and repeated reconciles converge.

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/jobbuilder"
)

// operatorVersionFallback is the archive record's operator_version when the
// operator was started without an OPERATOR_VERSION env: dev-stack operator
// images carry no version marker, and an archive record is never blocked on
// provenance it can honestly describe with this.
const operatorVersionFallback = "dev"

// isRunBackedRecord reports whether the record keys off a castle run id
// rather than the CR-uid derived synthetic key (the fallback path below
// only applies to run-keyed records).
func isRunBackedRecord(record *castlev1.ArchiveRecord) bool {
	return record.RunId != "" && record.ArchiveKey == ""
}

// ensureTerminalArchived archives the run's terminal record into castle (or
// confirms one is already stamped). It consumes the pass's castle
// observation for the run-record enrichment (workflow name/ref/digest,
// failure reason, timestamps, step summary); nil observations degrade to
// CR-derived fields. A nil error means the archive is in place and the
// caller may proceed to child-job reaping and finalizer removal; a returned
// error has already stamped TerminalArchivePending and the caller MUST
// abort this pass's cleanup.
func (r *CriteriaRunReconciler) ensureTerminalArchived(ctx context.Context, run *criteriav1.CriteriaRun, obs *castle.Observation, logger logr.Logger) error {
	if r.RunArchiver == nil || r.RunArchiver.Disabled() {
		// Archive lane not configured — no archiver wired, or the lane is
		// missing its castle address or its criteria token (castle rejects
		// anonymous archive writes, so either half missing would turn every
		// attempt into a permanent failure and wedge terminal cleanup; see
		// castle.RunArchiveClient.Disabled). The gate therefore skips and
		// terminal cleanup proceeds, the way it did before the archive leg
		// existed. The operator process logs the half-configured lane
		// loudly at startup, and the backfill mode refuses to claim
		// coverage over it.
		return nil
	}
	if run.Status.ArchiveID != "" && !run.Status.ArchivePending {
		return nil
	}

	record, _, err := r.buildTerminalArchiveRecord(ctx, run, obs)
	if err != nil {
		return r.markTerminalArchivePending(ctx, run, "ArchiveRecordBuildFailed",
			fmt.Sprintf("castle run archive not attempted (record build failed): %v", err))
	}

	resp, err := r.RunArchiver.ArchiveRun(ctx, connect.NewRequest(&castlev1.ArchiveRunRequest{Record: record}))
	if err != nil {
		// Run-backed keying refused: the run id either resolves to no known
		// castle run (a reset or pruned castle) or to a run this caller
		// identity does not own (e.g. runs registered by an anonymous
		// runner lane while the archive lane carries a token). The CR's
		// evidence never hangs on that bookkeeping disagreement: retry once
		// with the CR-uid derived key so the record still lands.
		if isRunBackedRecord(record) && string(run.UID) != "" &&
			(connect.CodeOf(err) == connect.CodeNotFound || connect.CodeOf(err) == connect.CodePermissionDenied) {
			if fbResp, fbErr := r.archiveWithCRUIDKey(ctx, run, record); fbErr == nil {
				return r.markTerminalArchived(ctx, run, fbResp.Msg.GetArchiveId())
			}
		}
		// Record content is never logged (ticket titles and repo URLs are
		// not for operator logs); ids and error codes are.
		return r.markTerminalArchivePending(ctx, run, "ArchiveRunFailed",
			fmt.Sprintf("castle run archive not yet stored (requeued with backoff): %v", err))
	}
	return r.markTerminalArchived(ctx, run, resp.Msg.GetArchiveId())
}

// archiveWithCRUIDKey re-keys a built record onto the CR's uid-derived
// synthetic archive key (run_id cleared — castle refuses records with both
// set) and archives it.
func (r *CriteriaRunReconciler) archiveWithCRUIDKey(ctx context.Context, run *criteriav1.CriteriaRun, record *castlev1.ArchiveRecord) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	uid := string(run.UID)
	if uid == "" {
		return nil, errors.New("run has no CastleRunID and no CR uid: no archive key can be derived")
	}
	record.ArchiveKey = castle.CRUIDArchiveKey(uid)
	record.RunId = ""
	req := connect.NewRequest(&castlev1.ArchiveRunRequest{Record: record})
	return r.RunArchiver.ArchiveRun(ctx, req)
}

// buildTerminalArchiveRecord maps the run (and, when available, the castle
// observation's run record enrichment) onto castle's ArchiveRecord contract,
// returning the record and its effective archive key (run_id, else the
// cr-uid key) for the caller's logging.
func (r *CriteriaRunReconciler) buildTerminalArchiveRecord(ctx context.Context, run *criteriav1.CriteriaRun, obs *castle.Observation) (*castlev1.ArchiveRecord, string, error) {
	if obs == nil {
		obs = &castle.Observation{}
	}

	runID := run.Status.CastleRunID
	if runID == "" {
		runID = obs.RunID
	}

	workflowName, workflowRef, treeDigest := "", "", ""
	if run.Spec.Workflow != nil {
		workflowName = run.Spec.Workflow.Name
		workflowRef = run.Spec.Workflow.Ref
	}
	if run.Spec.WorkflowSource != nil && run.Spec.WorkflowSource.Ref != "" {
		workflowRef = run.Spec.WorkflowSource.Ref
	}
	if obs.RunRecord != nil {
		if obs.RunRecord.WorkflowName != "" {
			workflowName = obs.RunRecord.WorkflowName
		}
		treeDigest = obs.RunRecord.WorkflowHash
	}
	// tree_digest has no CR-side source when castle enrichment is absent:
	// it stays empty rather than being guessed from the refs.

	effectiveKey := runID
	if runID == "" {
		uid := string(run.UID)
		if uid == "" {
			return nil, "", errors.New("no castle run id and no CR uid: no archive key can be derived")
		}
		effectiveKey = castle.CRUIDArchiveKey(uid)
	}
	if workflowName == "" && run.Spec.TicketID == "" && runID == "" {
		// A record with no identifying fields at all would archive garbage;
		// refuse instead of storing it (the pass stamps
		// TerminalArchivePending and requeues).
		return nil, "", errors.New("archive record would carry no ticket, workflow or castle run identity")
	}

	restarts, err := r.runnerPodRestarts(ctx, run)
	if err != nil {
		return nil, "", err
	}

	record := &castlev1.ArchiveRecord{
		Ticket:       run.Spec.TicketID,
		Repo:         run.Spec.RepoURL,
		JobName:      run.Status.JobName,
		WorkflowName: workflowName,
		WorkflowRef:  workflowRef,
		TreeDigest:   treeDigest,
		// Binding/project provenance: the run's config ConfigMap pin
		// (KB-103) is the only configmap-shaped provenance the operator
		// carries; it is name@resourceVersion, name alone when no RV was
		// stamped. The binding side stays empty — WorkflowGraphs bindings
		// are pinned digests, not ConfigMaps, so there is no operator
		// producer to fill binding_configmap_checksum from.
		ProjectConfigmapChecksum: projectConfigmapChecksum(run),
		FinalPhase:               string(run.Status.Phase),
		OperatorVersion:          r.operatorVersion(),
		FailureReason:            archiveFailureReason(run, obs),
		Restarts:                 int32(restarts),
		// backoff_evictions has no operator producer: nothing counts
		// runner Job evictions today, so zero is the honest value.
		BackoffEvictions: 0,
		TerminalCleanup:  archiveTerminalCleanupState(run),
		StartedAt:        archiveStartedAt(obs),
		EndedAt:          archiveEndedAt(obs),
	}
	if runID != "" {
		record.RunId = runID
	} else {
		record.ArchiveKey = effectiveKey
	}
	for i := range run.Status.Conditions {
		cond := &run.Status.Conditions[i]
		if cond.Type == criteriav1.ConditionTerminalArchivePending {
			// Gate bookkeeping, not run evidence.
			continue
		}
		record.Conditions = append(record.Conditions, &castlev1.ArchiveCondition{
			Type:               cond.Type,
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: castle.RFC3339ArchiveTime(cond.LastTransitionTime.Time),
		})
	}
	for _, step := range obs.Steps {
		record.Steps = append(record.Steps, &castlev1.ArchiveStep{Name: step.Name, Outcome: step.Outcome})
	}
	if verdict := archiveWorkflowVerdict(run, obs); verdict != "" {
		// The workflow's own verdict (SCOPE 1's finalState): the proto's
		// conditions list is where the record contract carries it. The CR
		// status is authoritative ("failed", "handler_complete",
		// "need_assistance" verbs); the castle terminal envelope backs it
		// up when the CR field is empty. Empty from both stays empty — a
		// verdict is never invented (e.g. the finalize()-with-nil-obs
		// deletion path on a still-Running run).
		record.Conditions = append(record.Conditions, &castlev1.ArchiveCondition{
			Type:    "FinalState",
			Status:  "True",
			Reason:  verdict,
			Message: "workflow final state: " + verdict,
		})
	}
	// The ceilings are enforced for real inside the archiver (the single
	// choke point on the wire); building capped bodies here keeps direct
	// callers (backfill, tests) equally honest.
	castle.ApplyArchiveRecordCeilings(record)
	return record, effectiveKey, nil
}

// archiveWorkflowVerdict resolves the run's workflow verdict for the record
// conditions list: the CR status' finalState, else the castle terminal
// envelope's final_state. Empty from both keeps the field out of the record.
func archiveWorkflowVerdict(run *criteriav1.CriteriaRun, obs *castle.Observation) string {
	if run.Status.FinalState != "" {
		return run.Status.FinalState
	}
	if obs.Terminal != nil && obs.Terminal.FinalState != "" {
		return obs.Terminal.FinalState
	}
	return ""
}

// projectConfigmapChecksum renders the run's KB-103 config ConfigMap pin as
// a provenance checksum: "name@resourceVersion", the name alone when the
// watcher stamped no RV. Nil Spec.ConfigRef stays empty.
func projectConfigmapChecksum(run *criteriav1.CriteriaRun) string {
	ref := run.Spec.ConfigRef
	if ref == nil || ref.Name == "" {
		return ""
	}
	if ref.ResourceVersion == "" {
		return ref.Name
	}
	return ref.Name + "@" + ref.ResourceVersion
}

// archiveFailureReason resolves the record's failure_reason: the terminal
// envelope's RunFailed reason when the run failed, else the castle run
// record's failure_reason. A failed-phase run with neither source records
// an empty reason rather than an invented one.
func archiveFailureReason(run *criteriav1.CriteriaRun, obs *castle.Observation) string {
	if run.Status.Phase != criteriav1.PhaseFailed {
		return ""
	}
	if obs.Terminal != nil && !obs.Terminal.Success {
		return obs.Terminal.Reason
	}
	if obs.RunRecord != nil {
		return obs.RunRecord.FailureReason
	}
	return ""
}

// archiveTerminalCleanupState mirrors the TerminalCleanup condition into the
// record's terminal_cleanup field: stamped-True means the run's terminal
// cleanup is (or was) deferred behind a pending runner restart; anything
// else reads as a cleanly completed cleanup.
func archiveTerminalCleanupState(run *criteriav1.CriteriaRun) string {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == ConditionTerminalCleanup && run.Status.Conditions[i].Status == metav1.ConditionTrue {
			return castle.TerminalCleanupDeferred
		}
	}
	return castle.TerminalCleanupCleared
}

// archiveStartedAt/EndedAt resolve the record's timestamps (RFC3339, empty
// when the source is unknown): start from the castle run record, end from
// the observed terminal (envelope or record) — never invented.
func archiveStartedAt(obs *castle.Observation) string {
	if obs.RunRecord != nil && !obs.RunRecord.StartedAt.IsZero() {
		return castle.RFC3339ArchiveTime(obs.RunRecord.StartedAt)
	}
	return ""
}

func archiveEndedAt(obs *castle.Observation) string {
	if obs.Terminal != nil && !obs.Terminal.At.IsZero() {
		return castle.RFC3339ArchiveTime(obs.Terminal.At)
	}
	if obs.RunRecord != nil && !obs.RunRecord.EndedAt.IsZero() {
		return castle.RFC3339ArchiveTime(obs.RunRecord.EndedAt)
	}
	return ""
}

// runnerPodRestarts sums the runner pods' container restart counts (the
// record's restarts field: lifecycle evidence of how the run's runner
// container fared). Missing or already-deleted runners contribute zero.
func (r *CriteriaRunReconciler) runnerPodRestarts(ctx context.Context, run *criteriav1.CriteriaRun) (int, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(run.Namespace),
		client.MatchingLabels(map[string]string{
			jobbuilder.LabelRun:  run.Name,
			jobbuilder.LabelRole: jobbuilder.RoleRunner,
		}),
	); err != nil {
		return 0, fmt.Errorf("listing runner pods for the archive record: %w", err)
	}
	total := 0
	for i := range pods.Items {
		for _, cs := range pods.Items[i].Status.ContainerStatuses {
			total += int(cs.RestartCount)
		}
		for _, cs := range pods.Items[i].Status.InitContainerStatuses {
			total += int(cs.RestartCount)
		}
	}
	return total, nil
}

// operatorVersion resolves the record's operator_version: the reconciler's
// OPERATOR_VERSION value when set, the dev fallback otherwise.
func (r *CriteriaRunReconciler) operatorVersion() string {
	if r.OperatorVersion != "" {
		return r.OperatorVersion
	}
	return operatorVersionFallback
}

// markTerminalArchivePending stamps the TerminalArchivePending condition
// (with a fresh re-read so the write carries the run's latest
// resourceVersion) and returns the error the caller aborts the pass with.
func (r *CriteriaRunReconciler) markTerminalArchivePending(ctx context.Context, run *criteriav1.CriteriaRun, reason, message string) error {
	logf.FromContext(ctx).Error(errors.New(message), "terminal archive gate failed",
		"criteriarun", run.Name, "reason", reason)
	key := types.NamespacedName{Name: run.Name, Namespace: run.Namespace}
	var fresh criteriav1.CriteriaRun
	if err := r.Get(ctx, key, &fresh); err != nil {
		return fmt.Errorf("re-reading CriteriaRun for the %s condition: %w", criteriav1.ConditionTerminalArchivePending, err)
	}
	update := fresh.DeepCopy()
	upsertCondition(update, metav1.Condition{
		Type:               criteriav1.ConditionTerminalArchivePending,
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: fresh.Generation,
	})
	update.Status.ArchivePending = true
	if !statusEqual(&fresh.Status, &update.Status) {
		if err := r.Status().Update(ctx, update); err != nil {
			return fmt.Errorf("stamping %s condition: %w", criteriav1.ConditionTerminalArchivePending, err)
		}
	}
	return errors.New(message)
}

// markTerminalArchived stamps the landed archive's id (and clears the
// pending state) after a fresh re-read.
func (r *CriteriaRunReconciler) markTerminalArchived(ctx context.Context, run *criteriav1.CriteriaRun, archiveID string) error {
	key := types.NamespacedName{Name: run.Name, Namespace: run.Namespace}
	var fresh criteriav1.CriteriaRun
	if err := r.Get(ctx, key, &fresh); err != nil {
		return fmt.Errorf("re-reading CriteriaRun for the archive stamp: %w", err)
	}
	update := fresh.DeepCopy()
	update.Status.ArchiveID = archiveID
	update.Status.ArchivePending = false
	// The pending condition describes an unarchived terminal run; with a
	// record stored it has said its piece.
	for i := range update.Status.Conditions {
		if update.Status.Conditions[i].Type == criteriav1.ConditionTerminalArchivePending {
			update.Status.Conditions[i].Status = metav1.ConditionFalse
			update.Status.Conditions[i].Reason = "Archived"
			update.Status.Conditions[i].Message = "castle run archive stored: " + archiveID
		}
	}
	if !statusEqual(&fresh.Status, &update.Status) {
		if err := r.Status().Update(ctx, update); err != nil {
			return fmt.Errorf("stamping archive id into CriteriaRun status: %w", err)
		}
	}
	return nil
}
