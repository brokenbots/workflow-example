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

// RunQueue is an operator-level, FIFO admission queue keyed by repo URL.
// It ensures that at most one CriteriaRun per repo is executing at a time.
type RunQueue struct {
	mu      sync.Mutex
	queues  map[string][]queueEntry
	running map[string]types.NamespacedName
}

// NewRunQueue creates an empty RunQueue.
func NewRunQueue() *RunQueue {
	return &RunQueue{
		queues:  make(map[string][]queueEntry),
		running: make(map[string]types.NamespacedName),
	}
}

// Enqueue registers the run for its repoURL and returns admission state along
// with repo-keyed queue status. If no run is currently admitted for the repo,
// the head of the queue is admitted. When the run is queued behind another run,
// prevRunning is the currently admitted run so the caller can refresh its
// queue status.
func (q *RunQueue) Enqueue(run *criteriav1.CriteriaRun) (admitted bool, status *criteriav1.CriteriaRunQueueStatus, prevRunning *types.NamespacedName) {
	q.mu.Lock()
	defer q.mu.Unlock()

	repo := run.Spec.RepoURL
	key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}

	entries := q.queues[repo]
	if indexOf(entries, key) == -1 {
		entries = append(entries, queueEntry{key: key})
		q.queues[repo] = entries
	}

	// Admit the queue head whenever the repo is not already running.
	if _, busy := q.running[repo]; !busy && len(entries) > 0 {
		q.running[repo] = entries[0].key
	}

	admitted = q.running[repo] == key
	if !admitted {
		runKey := q.running[repo]
		if runKey != (types.NamespacedName{}) {
			prevRunning = &runKey
		}
	}
	return admitted, q.statusLocked(repo, key, entries), prevRunning
}

// Release removes a run from its repo queue. If the removed run was the
// currently admitted run, the next queued run is admitted and returned so
// the caller can reconcile it.
func (q *RunQueue) Release(key types.NamespacedName, repo string) *types.NamespacedName {
	q.mu.Lock()
	defer q.mu.Unlock()

	entries := q.queues[repo]
	idx := indexOf(entries, key)
	if idx == -1 {
		return nil
	}

	entries = append(entries[:idx], entries[idx+1:]...)
	if len(entries) == 0 {
		delete(q.queues, repo)
	} else {
		q.queues[repo] = entries
	}

	if q.running[repo] == key {
		delete(q.running, repo)
	}

	if len(entries) > 0 {
		if _, busy := q.running[repo]; !busy {
			next := entries[0].key
			q.running[repo] = next
			return &next
		}
	}
	return nil
}

// Recover populates the queue from existing non-terminal CriteriaRuns. It is
// intended for operator startup so in-flight runs are not lost across restarts.
func (q *RunQueue) Recover(ctx context.Context, cl client.Client) error {
	var list criteriav1.CriteriaRunList
	if err := cl.List(ctx, &list); err != nil {
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

		repo := run.Spec.RepoURL
		key := types.NamespacedName{Namespace: run.Namespace, Name: run.Name}
		if indexOf(q.queues[repo], key) == -1 {
			q.queues[repo] = append(q.queues[repo], queueEntry{key: key})
		}
	}

	for repo, entries := range q.queues {
		if _, busy := q.running[repo]; !busy && len(entries) > 0 {
			q.running[repo] = entries[0].key
		}
	}

	return nil
}

func (q *RunQueue) statusLocked(repo string, key types.NamespacedName, entries []queueEntry) *criteriav1.CriteriaRunQueueStatus {
	runKey := q.running[repo]
	status := &criteriav1.CriteriaRunQueueStatus{
		RepoURL: repo,
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

func indexOf(entries []queueEntry, key types.NamespacedName) int {
	for i, e := range entries {
		if e.key == key {
			return i
		}
	}
	return -1
}
