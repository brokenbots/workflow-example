package controller

import (
	"context"
	"fmt"
	"os"

	criteriav1 "github.com/brokenbots/workflow-example/criteria-k8s/api/v1"
)

// FileEventsReader reads a run's events file from the local filesystem. The
// operator deployment mounts the shared /data PVC at DataRoot so it can tail
// the same events stream that the runner writes.
type FileEventsReader struct {
	DataRoot string
}

// Read returns the raw bytes of the run's events file. A missing file is
// treated as empty so active runs that have not yet written any events do not
// cause spurious errors.
func (r *FileEventsReader) Read(ctx context.Context, run *criteriav1.CriteriaRun) ([]byte, error) {
	path := eventsPath(run)
	if r.DataRoot != "" {
		path = fmt.Sprintf("%s%s", r.DataRoot, eventsPathRelative(run))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading events file %s: %w", path, err)
	}
	return data, nil
}

func eventsPathRelative(run *criteriav1.CriteriaRun) string {
	return fmt.Sprintf("/intake/%s/events.ndjson", run.Spec.TicketID)
}
