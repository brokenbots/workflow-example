package controller

import (
	"context"
	"sync"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// queueEntry tracks a single CriteriaRun waiting for or admitted to a repo queue.
type queueEntry struct {
	key types.NamespacedName
}

// queueKey identifies one admission queue: a (repoURL, class) pair
// (CRI-242). dev-class runs serialize per repoURL; triage-class runs are
// read-only against the repo and admit concurrently with any run on the
// same repoURL, so their queues are distinct.
type queueKey struct {
	repo  string
	class string
}

// RunQueue is an operator-level, FIFO admission queue keyed by
// (repo URL, class). It ensures that at most one dev-class CriteriaRun per
// repo is executing at a time; triage-class runs may run concurrently with
// any run on the same repo.
type RunQueue struct {
	mu      sync.Mutex
	queues  map[queueKey][]queueEntry
	running map[queueKey]types.NamespacedName
}

// NewRunQueue creates an empty RunQueue.
func NewRunQueue() *RunQueue {
	return &RunQueue{
		queues:  make(map[queueKey][]queueEntry),
		running: make(map[queueKey]types.NamespacedName),
	}
}

// Enqueue registers the run for its (repoURL, class) queue and returns
// admission state along with queue status. If no run is currently admitted
// for the queue, the head of the queue is admitted. When the run is queued
// behind another run, prevRunning is the currently admitted run so the
// caller can refresh its queue status.
func (q *RunQueue) Enqueue(run *criteriav1.CriteriaRun) (admitted bool, status *criteriav1.CriteriaRunQueueStatus, prevRunning *types.NamespacedName) {
	q.mu.Lock()
	defer q.mu.Unlock()

	qk := queueKeyOf(run)
	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}

	entries := q.queues[qk]
	if indexOf(entries, key) == -1 {
		entries = append(entries, queueEntry{key: key})
		q.queues[qk] = entries
	}

	// Admit the queue head whenever the queue is not already running.
	if _, busy := q.running[qk]; !busy && len(entries) > 0 {
		q.running[qk] = entries[0].key
	}

	admitted = q.running[qk] == key
	if !admitted {
		runKey := q.running[qk]
		if runKey != (types.NamespacedName{}) {
			prevRunning = &runKey
		}
	}
	return admitted, q.statusLocked(qk, key, entries), prevRunning
}

// Release removes a run from its (repoURL, class) queue. If the removed run
// was the currently admitted run, the next queued run is admitted and
// returned so the caller can reconcile it.
func (q *RunQueue) Release(run *criteriav1.CriteriaRun) *types.NamespacedName {
	q.mu.Lock()
	defer q.mu.Unlock()

	qk := queueKeyOf(run)
	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}

	entries := q.queues[qk]
	idx := indexOf(entries, key)
	if idx == -1 {
		return nil
	}

	entries = append(entries[:idx], entries[idx+1:]...)
	if len(entries) == 0 {
		delete(q.queues, qk)
	} else {
		q.queues[qk] = entries
	}

	if q.running[qk] == key {
		delete(q.running, qk)
	}

	if len(entries) > 0 {
		if _, busy := q.running[qk]; !busy {
			next := entries[0].key
			q.running[qk] = next
			return &next
		}
	}
	return nil
}

// Recover populates the queue from existing non-terminal CriteriaRuns. It is
// intended for operator startup so in-flight runs are not lost across restarts.
// The list is namespace-scoped to match the operator's namespaced RBAC.
func (q *RunQueue) Recover(ctx context.Context, cl client.Client, namespace string) error {
	list := &criteriav1.CriteriaRunList{}
	if err := cl.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return err
	}

	q.mu.Lock()
	defer q.mu.Unlock()

	for i := range list.Items {
		run := &list.Items[i]
		if run.DeletionTimestamp != nil {
			continue
		}
		if isTerminalPhase(run.Status.Phase) {
			continue
		}

		qk := queueKeyOf(run)
		key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}
		if indexOf(q.queues[qk], key) == -1 {
			q.queues[qk] = append(q.queues[qk], queueEntry{key: key})
		}
	}

	for qk, entries := range q.queues {
		if _, busy := q.running[qk]; !busy && len(entries) > 0 {
			q.running[qk] = entries[0].key
		}
	}

	return nil
}

func (q *RunQueue) statusLocked(qk queueKey, key types.NamespacedName, entries []queueEntry) *criteriav1.CriteriaRunQueueStatus {
	runKey := q.running[qk]
	status := &criteriav1.CriteriaRunQueueStatus{
		RepoURL: qk.repo,
		Class:   qk.class,
		Length:  len(entries),
	}
	if runKey != (types.NamespacedName{}) {
		status.Running = runKey.Name
	}

	position := 0
	for _, e := range entries {
		if e.key == runKey {
			continue
		}
		if e.key == key {
			position = len(status.Pending) + 1
		}
		status.Pending = append(status.Pending, e.key.Name)
	}
	status.Position = position
	return status
}

// queueKeyOf resolves the run's admission queue key (CRI-242): the repoURL
// plus the admission class derived from the stamped workflow object.
func queueKeyOf(run *criteriav1.CriteriaRun) queueKey {
	return queueKey{repo: run.Spec.RepoURL, class: admissionClass(run)}
}

// admissionClass resolves the run's queue class (CRI-242): "triage" only
// when the stamped workflow object declares it explicitly. Runs without a
// stamped workflow — and workflows without an explicit class, or with an
// unrecognized value — behave as dev, preserving the per-repo
// serialization that predated classes.
func admissionClass(run *criteriav1.CriteriaRun) string {
	if run.Spec.Workflow != nil && run.Spec.Workflow.Class == criteriav1.RunClassTriage {
		return criteriav1.RunClassTriage
	}
	return criteriav1.RunClassDev
}

func indexOf(entries []queueEntry, key types.NamespacedName) int {
	for i, e := range entries {
		if e.key == key {
			return i
		}
	}
	return -1
}
