package controller

// CriteriaRun retention (KB-309): archived terminal runs age out.
//
// The terminal CriteriaRuns the operator keeps are its memory sink: run
// records plus the child Jobs/Pods they retain pile up in the API server's
// live data (~95KB per run incl. Jobs+Pods history). With the KB-314 castle
// archive leg in place, deleting a run loses nothing — the castle
// run-archive holds the full record. A background sweep deletes runs that
// are
//   - terminal (isTerminalPhase: Succeeded or Failed),
//   - not parked for human attention (finalState need_assistance, or the
//     pre-KB-269 "awaiting_human" spelling — a parked run is a human's
//     pending work and is NEVER auto-deleted, archived or not),
//   - fully archived (ArchiveID set and ArchivePending false — the KB-314
//     stamp; a run the archive gate is still working on stalls deletion,
//     fail-safe),
//   - older than the retention window (status.terminalAt; an unstamped
//     terminal run has no known age and is never a candidate), and
//   - not one of the newest keep-last terminal runs of its ticket family
//     (spec.ticketId — failure forensics want recent siblings regardless of
//     age; parked runs hold their family's window like any other terminal
//     run).
//
// Deletion cascades the child Jobs and Pods via the run's ownerReferences
// (kubectl-style k8s GC plus the reconcile's finalize, which explicitly
// deletes the runner/adapter Jobs under the finalizer). The sweep then
// verifies nothing was left ownerless: a Job or Pod whose CriteriaRun owner
// is unrecoverable (no same-name run, and no trace in the pass's snapshot)
// is deleted as well; the delete decision re-reads the owner from the API
// at delete time, so a run admitted after the pass's snapshot is proven
// live by its Get and its children are never touched — UID equality only,
// so recycled names never adopt the dead owner's objects, mirroring the
// adapter sweep.
//
// Deletion is opt-in per namespace (the allowlist from
// --cr-retention-namespaces): a namespace must be listed for anything to be
// deleted, so cutovers turn retention on per namespace after their archive
// leg round-trip verifies.
import (
	"context"
	"sort"
	"strings"
	"time"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"github.com/brokenbots/workflow-example/criteria-k8s/internal/castle"
	logr "github.com/go-logr/logr"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// DefaultCRRetentionDays and DefaultCRRetentionKeepLast are the flag and
	// env-var defaults (--cr-retention-days / CR_RETENTION_DAYS,
	// --cr-retention-keep-last / CR_RETENTION_KEEP_LAST).
	DefaultCRRetentionDays     = 7
	DefaultCRRetentionKeepLast = 3

	// Parked final states: the run handed the ticket to a human and the run
	// record (with its bookkeeping) stays visible until the human closes it
	// out. Retention never deletes a parked run.
	crParkedFinalStateNeedAssistance = "need_assistance"
	// pre-KB-269 spelling, still valid while old pinned trees emit it.
	crParkedFinalStateAwaitingHuman = "awaiting_human"
)

// stampTerminalAt pins the run's terminal clock (KB-309) into the status
// update that carries its terminal phase: once, on the transition, before
// the CR's terminal pass runs the archive gate. The stamp prefers the castle
// terminal envelope's own timestamp (the truthful terminal time — stable
// across reconciles and operator restarts, and the only source that ages the
// KB-314 backlog of already-terminal runs truthfully), falling back to the
// run record's ended-at and then the reconcile clock. Callers without an
// observation pass nil; the stamp only ever happens via the no-op-on-set
// guard, so re-reconciles never rewrite it.
func stampTerminalAt(update *criteriav1.CriteriaRun, obs *castle.Observation) {
	if update == nil || !isTerminalPhase(update.Status.Phase) || update.Status.TerminalAt != nil {
		return
	}
	at := time.Now()
	if obs != nil && obs.Terminal != nil && !obs.Terminal.At.IsZero() {
		at = obs.Terminal.At
	} else if obs != nil && obs.RunRecord != nil && !obs.RunRecord.EndedAt.IsZero() {
		at = obs.RunRecord.EndedAt
	}
	update.Status.TerminalAt = &metav1.Time{Time: at}
}

// crFinalStateParked reports whether the run's final state parked the
// ticket for human attention.
func crFinalStateParked(finalState string) bool {
	switch strings.TrimSpace(finalState) {
	case crParkedFinalStateNeedAssistance, crParkedFinalStateAwaitingHuman:
		return true
	}
	return false
}

// CRRetentionSweeper deletes archived terminal CriteriaRuns older than the
// retention window (KB-309) and reaps any child objects their deletion left
// ownerless.
type CRRetentionSweeper struct {
	client.Client
	// Namespace is the managed namespace the sweep runs against.
	Namespace string
	// Retention is the terminal-age window (from --cr-retention-days).
	Retention time.Duration
	// KeepLast is the per-ticket-family window of newest terminal runs the
	// retention never deletes (from --cr-retention-keep-last).
	KeepLast int
	// Allowlist is the deletion opt-in: the managed namespace must be
	// listed for the sweep to delete anything.
	Allowlist map[string]struct{}
	// Interval between sweeps; a sweep also runs once at startup.
	Interval time.Duration
}

// NewCRRetentionSweeper builds the retention sweep from flag/env inputs.
// Whitespace in the allowlist is trimmed; empty entries are dropped. A
// non-positive interval is clamped to the 1h default so flag/env input
// (--retention-interval=0 is a disabled-sweep idiom elsewhere) can never
// panic time.NewTicker in the sweep goroutine.
func NewCRRetentionSweeper(cl client.Client, namespace string, retention time.Duration, keepLast int, namespaces []string, interval time.Duration) *CRRetentionSweeper {
	allowlist := map[string]struct{}{}
	for _, ns := range namespaces {
		if ns = strings.TrimSpace(ns); ns != "" {
			allowlist[ns] = struct{}{}
		}
	}
	if keepLast < 0 {
		keepLast = 0
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &CRRetentionSweeper{
		Client:    cl,
		Namespace: namespace,
		Retention: retention,
		KeepLast:  keepLast,
		Allowlist: allowlist,
		Interval:  interval,
	}
}

// Run launches the sweep loop. Sweeps also run once at startup so a backlog
// walks immediately, mirroring the retention/archive sweep conventions.
func (s *CRRetentionSweeper) Run(ctx context.Context) {
	logger := logf.FromContext(ctx).WithValues("component", "cr-retention-sweep", "namespace", s.Namespace)
	logger.Info("starting criteria run retention sweep",
		"retention", s.Retention.String(), "keepLast", s.KeepLast,
		"allowlist", s.Allowlist, "interval", s.Interval.String())
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	s.sweep(ctx, logger)
	for {
		select {
		case <-ctx.Done():
			logger.Info("stopping criteria run retention sweep")
			return
		case <-ticker.C:
			s.sweep(ctx, logger)
		}
	}
}

// crRetentionStats counts a pass's outcomes for the per-pass log line.
type crRetentionStats struct {
	terminal, parked, unarchived, unstamped, keptByFamily, deleting, deleted, orphanJobs, orphanPods int
}

func (s *CRRetentionSweeper) sweep(ctx context.Context, logger logr.Logger) {
	now := time.Now()
	// Deletion is opt-in per namespace: the allowlist is the only switch
	// that arms it, and it gates the whole pass (deletes and the orphan
	// verify alike).
	if _, ok := s.Allowlist[s.Namespace]; !ok {
		logger.V(1).Info("criteria run retention: namespace not in the deletion allowlist; skipping", "retention", s.Retention.String())
		return
	}

	runs, err := s.listRuns(ctx, s.Namespace)
	if err != nil {
		logger.Error(err, "listing CriteriaRuns for retention sweep")
		return
	}

	uidSet := make(map[types.UID]struct{}, len(runs))
	for i := range runs {
		uidSet[runs[i].UID] = struct{}{}
	}

	// Per ticket family, newest wins the keep-last window. Unstamped runs
	// rank oldest: their terminal age is unknown, so they must never
	// displace a stamped (ageable) run from the protected window — the
	// conservative direction.
	familyOrder := make(map[string][]*criteriav1.CriteriaRun)
	for i := range runs {
		run := &runs[i]
		if !isTerminalPhase(run.Status.Phase) {
			continue
		}
		familyOrder[run.Spec.TicketID] = append(familyOrder[run.Spec.TicketID], run)
	}
	kept := make(map[types.UID]struct{})
	for _, members := range familyOrder {
		members := append([]*criteriav1.CriteriaRun(nil), members...)
		sort.Slice(members, func(i, j int) bool {
			ti, tj := crTerminalRankTime(members[i]), crTerminalRankTime(members[j])
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
			ci, cj := members[i].CreationTimestamp.Time, members[j].CreationTimestamp.Time
			if !ci.Equal(cj) {
				return ci.After(cj)
			}
			return members[i].Name < members[j].Name
		})
		for k, run := range members {
			if k < s.KeepLast {
				kept[run.UID] = struct{}{}
			}
		}
	}

	stats := &crRetentionStats{}
	for i := range runs {
		run := &runs[i]
		if !isTerminalPhase(run.Status.Phase) {
			continue
		}
		stats.terminal++
		// Parked runs never age out, archived or not: need_assistance is
		// human attention pending, not waste.
		if crFinalStateParked(run.Status.FinalState) {
			stats.parked++
			logger.V(1).Info("criteria run retained: parked for human attention", "run", run.Name)
			continue
		}
		// Fail-safe: the archive gate has not landed this run's castle
		// record (its TerminalArchivePending condition carries the
		// reason). Deleting now would lose the only copy.
		if run.Status.ArchiveID == "" || run.Status.ArchivePending {
			stats.unarchived++
			logger.V(1).Info("criteria run retained: castle archive record pending (TerminalArchivePending)", "run", run.Name)
			continue
		}
		// No terminal clock: the age is unknown, so the retention window
		// cannot be evaluated (legacy terminal runs until the first
		// post-rollout reconcile).
		if run.Status.TerminalAt == nil {
			stats.unstamped++
			logger.V(1).Info("criteria run retained: no terminal timestamp", "run", run.Name)
			continue
		}
		if _, ok := kept[run.UID]; ok {
			stats.keptByFamily++
			logger.V(1).Info("criteria run retained: within the per-family keep-last window", "run", run.Name)
			continue
		}
		if now.Sub(run.Status.TerminalAt.Time) < s.Retention {
			continue
		}
		if run.DeletionTimestamp != nil {
			continue
		}
		// The finalizer turns the delete into a Reconcile (finalize
		// deletes the child Jobs explicitly), k8s GC cascades the rest
		// through ownerReferences; the orphan verify below is the safety
		// net.
		if err := s.Delete(ctx, run); err != nil && !apierrors.IsNotFound(err) {
			logger.Error(err, "deleting aged terminal CriteriaRun", "run", run.Name)
			continue
		}
		stats.deleted++
		logger.Info("deleted aged terminal CriteriaRun", "run", run.Name,
			"phase", run.Status.Phase, "terminalAt", run.Status.TerminalAt.Time, "age", now.Sub(run.Status.TerminalAt.Time).String())
	}

	// Orphan verify: every Job/Pod in the namespace whose CriteriaRun
	// ownerReference points at a UID no longer in the namespace is
	// reaped, so "deleted 100 runs ⇒ zero ownerless Jobs/Pods" holds even
	// when a finalize was skipped or a CR was force-deleted out of band.
	s.sweepOrphans(ctx, logger, uidSet, stats)

	logger.Info("criteria run retention sweep complete",
		"terminal", stats.terminal, "parked", stats.parked,
		"unarchived", stats.unarchived, "unstamped", stats.unstamped,
		"keptByFamily", stats.keptByFamily, "deleted", stats.deleted,
		"orphanJobsDeleted", stats.orphanJobs, "orphanPodsDeleted", stats.orphanPods)
}

// sweepOrphans reaps Jobs and Pods whose CriteriaRun ownerReference points
// at a run that no longer exists, so "deleted 100 runs ⇒ zero ownerless
// Jobs/Pods" holds even when a finalize was skipped or a CR was
// force-deleted out of band. The delete decision re-reads the owning run
// from the API at delete time (the AdapterSweeper race pattern): a run
// admitted after the pass's snapshot — or any owner a stale snapshot cannot
// see — is proven live by its Get, never treated as orphaned. The snapshot
// UIDs are only the grace signal for runs deleted in this same pass (their
// children may not be gone yet; the next pass reaps whatever leaked).
func (s *CRRetentionSweeper) sweepOrphans(ctx context.Context, logger logr.Logger, snapshot map[types.UID]struct{}, stats *crRetentionStats) {
	var jobs batchv1.JobList
	if err := s.Client.List(ctx, &jobs, client.InNamespace(s.Namespace)); err != nil {
		logger.Error(err, "listing Jobs for retention orphan verify")
		return
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if s.orphanVerify(ctx, criteriaRunOwnerRefOf(job), snapshot) {
			if err := s.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationForeground)); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "deleting orphaned child Job", "job", job.Name)
				continue
			}
			stats.orphanJobs++
			logger.Info("deleted orphaned child Job", "job", job.Name, "run", criteriaRunOwnerRefOf(job).Name)
		}
	}

	var pods corev1.PodList
	if err := s.Client.List(ctx, &pods, client.InNamespace(s.Namespace)); err != nil {
		logger.Error(err, "listing Pods for retention orphan verify")
		return
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if s.orphanVerify(ctx, criteriaRunOwnerRefOf(pod), snapshot) {
			if err := s.Client.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
				logger.Error(err, "deleting orphaned child Pod", "pod", pod.Name)
				continue
			}
			stats.orphanPods++
			logger.Info("deleted orphaned child Pod", "pod", pod.Name, "run", criteriaRunOwnerRefOf(pod).Name)
		}
	}
}

// orphanVerify decides whether the object's CriteriaRun owner is genuinely
// gone. A same-name run with a matching UID is live (the Get is
// authoritative even when the owner is missing from the pass's pre-delete
// snapshot); a UID in the snapshot is a run this pass just deleted (grace);
// everything else — an unresolvable name, a recycled name under a dead UID,
// or a UID absent both from the namespace and the snapshot — is orphaned.
// No match and no snapshot trace with a read error is kept (fail-safe).
func (s *CRRetentionSweeper) orphanVerify(ctx context.Context, ref *metav1.OwnerReference, snapshot map[types.UID]struct{}) bool {
	if ref == nil {
		return false
	}
	var owner criteriav1.CriteriaRun
	err := s.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: ref.Name}, &owner)
	if err == nil {
		return owner.UID != ref.UID // a recycled name under a dead UID
	}
	if !apierrors.IsNotFound(err) {
		return false
	}
	return !isCRUIDLive(snapshot, ref.UID)
}

// isCRUIDLive matches on UID — the identity that survives name recycling.
func isCRUIDLive(live map[types.UID]struct{}, uid types.UID) bool {
	_, ok := live[uid]
	return ok
}

// crTerminalRankTime is the retention/family ordering key: the stamped
// terminal time when there is one; the zero time for unstamped runs so they
// rank oldest and never displace a stamped run from the keep-last window.
func crTerminalRankTime(run *criteriav1.CriteriaRun) time.Time {
	if run.Status.TerminalAt != nil {
		return run.Status.TerminalAt.Time
	}
	return time.Time{}
}

func (s *CRRetentionSweeper) listRuns(ctx context.Context, namespace string) ([]criteriav1.CriteriaRun, error) {
	var all []criteriav1.CriteriaRun
	for cont := ""; ; {
		var page criteriav1.CriteriaRunList
		opts := []client.ListOption{client.InNamespace(namespace)}
		if cont != "" {
			opts = append(opts, client.Continue(cont))
		}
		if err := s.Client.List(ctx, &page, opts...); err != nil {
			return nil, err
		}
		all = append(all, page.Items...)
		if cont = page.ListMeta.Continue; cont == "" {
			return all, nil
		}
	}
}
