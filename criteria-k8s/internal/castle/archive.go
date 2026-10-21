package castle

// Run-archive write leg (KB-314, castle #308 leg 2): the operator's client
// for castle's RunArchiveService. Every terminal CriteriaRun must yield a
// castle archive record before child-job reaping and finalizer removal, so
// #309's retention can delete castle run history and CRs without losing the
// evidence. The wire contract is castle's gen surface (same module as the
// podstate feed); this file adds only the operator-side posture on top: the
// token lane (never anonymous), fail-closed misconfiguration, and the
// per-record body ceilings castle's unbounded-text store expects the
// writer to enforce.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

const (
	// runArchiveKeyPrefix is the prefix castle's store hangs archive ids off
	// (store.ArchiveID(key) = "run-archive/"+key); the operator stamps the
	// response's archive_id into CR status and compares against this form.
	runArchiveKeyPrefix = "run-archive/"

	// CRUIDArchiveKeyPrefix is the documented namespace for run-id-less
	// archive records (terminal CRs with no known castle overseer run):
	// castle rejects any other archive_key for a record that names no run.
	CRUIDArchiveKeyPrefix = "cr-uid/"

	// TerminalCleanupDeferred/TerminalCleanupCleared mirror the
	// TerminalCleanup condition's two outcomes in the archive record's
	// terminal_cleanup field.
	TerminalCleanupDeferred = "deferred"
	TerminalCleanupCleared  = "cleared"

	// Per-record body ceilings (KB-308 deferred concern, landed here): the
	// castle archive store persists record bodies as unbounded text, so the
	// WRITER enforces the ceiling. The step cap is the interesting one — a
	// pathologically long workflow would otherwise carry every step outcome
	// in one record — the rest bound free-form fields (condition messages,
	// failure reasons, ids) that the run's own event stream could blow up.
	MaxArchiveSteps      = 256
	MaxArchiveConditions = 32

	maxArchiveStepNameCap        = 256
	maxArchiveStepOutcomeCap     = 64
	maxArchiveConditionTypeCap   = 128
	maxArchiveCondReasonCap      = 128
	maxArchiveConditionMsgCap    = 512
	maxArchiveFailureReasonCap   = 2048
	maxArchiveWorkflowNameCap    = 128
	maxArchiveWorkflowRefCap     = 512
	maxArchiveRepoCap            = 256
	maxArchiveTicketCap          = 128
	maxArchiveJobNameCap         = 64
	maxArchiveRunIDCap           = 128
	maxArchiveArchiveKeyCap      = 128
	maxArchiveTreeDigestCap      = 256
	maxArchiveChecksumCap        = 128
	maxArchiveOperatorVerCap     = 64
	maxArchiveFinalPhaseCap      = 32
	maxArchiveTimestampCap       = 64
	maxArchiveTerminalCleanupCap = 16
)

// RunArchiver is the archive surface the controller consumes; tests fake it
// wholesale.
type RunArchiver interface {
	// ArchiveRun archives one terminal run's record (castle RunArchive-
	// Service.ArchiveRun). Idempotent on the record's effective archive key.
	ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error)
	// ListRunArchives pages stored archives; the backfill's coverage check
	// walks it (and wire tests round-trip through it).
	ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error)
	// Disabled reports that the archive leg is off (no castle address), so
	// the controller's terminal gate skips instead of retrying forever.
	Disabled() bool
}

// RunArchiverConfig configures the castle run-archive client.
type RunArchiverConfig struct {
	Addr  string
	Token string
}

// RunArchiveClient archives terminal run records into castle. It rides the
// same token lane as the podstate feed (X-Criteria-Token with the Bearer
// mirror), never anonymous: an unauthenticated archive write is rejected
// server-side, and this client refuses to even attempt one when no token is
// configured, so a token-less rollout fails loudly (the gate stamps
// TerminalArchivePending and requeues) instead of surfacing a server
// PermissionDenied that can be misread as an authorization policy decision.
type RunArchiveClient struct {
	addr       string
	token      string
	httpClient *http.Client
}

// NewRunArchiveClient builds the archive client. A nil httpClient gets the
// cleartext h2c transport, matching the rest of the castle lane; a
// configured token rides it on the standard criteria lane (mirroring the
// observation client — a checked-but-never-sent token would surface as a
// server PermissionDenied the gate would retry forever, so the lane is
// attached here, not left to the caller).
func NewRunArchiveClient(cfg RunArchiverConfig, httpClient *http.Client) *RunArchiveClient {
	if httpClient == nil {
		httpClient = newClearTextH2CClient()
	}
	if cfg.Token != "" {
		httpClient = &http.Client{Transport: &tokenTransport{base: httpClient.Transport, token: cfg.Token}}
	}
	return &RunArchiveClient{addr: cfg.Addr, token: cfg.Token, httpClient: httpClient}
}

// Disabled reports that no castle address is configured; the archiver is a
// no-op and the controller's terminal gate skips (pre-castle-archival
// deploys keep reconciling).
func (a *RunArchiveClient) Disabled() bool {
	return a == nil || a.addr == ""
}

func (a *RunArchiveClient) archiveServiceClient() castlev1connect.RunArchiveServiceClient {
	return castlev1connect.NewRunArchiveServiceClient(a.httpClient, a.addr)
}

func (a *RunArchiveClient) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	if a.Disabled() {
		return nil, errors.New("castle run archive is disabled (no castle address configured)")
	}
	if a.token == "" {
		return nil, errors.New("castle run archive requires a criteria token (CASTLE_TOKEN): refusing an anonymous archive write")
	}
	ApplyArchiveRecordCeilings(req.Msg.GetRecord())
	return a.archiveServiceClient().ArchiveRun(ctx, req)
}

func (a *RunArchiveClient) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	if a.Disabled() {
		return nil, errors.New("castle run archive is disabled (no castle address configured)")
	}
	return a.archiveServiceClient().ListRunArchives(ctx, req)
}

// RunArchiveID returns the archive id castle stamps for an effective archive
// key (store.ArchiveID): "run-archive/" + key. Status comparisons use it.
func RunArchiveID(key string) string { return runArchiveKeyPrefix + key }

// CRUIDArchiveKey derives a run-id-less record's synthetic archive key from
// the CR the record describes.
func CRUIDArchiveKey(uid string) string { return CRUIDArchiveKeyPrefix + uid }

// RFC3339ArchiveTime formats a timestamp for the record's string timestamp
// fields (empty when unset).
func RFC3339ArchiveTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ApplyArchiveRecordCeilings enforces the per-record body ceilings in place:
// free-form fields are truncated to their caps and oversized step/condition
// lists keep their FIRST entries (the oldest steps are the ones a debugging
// reviewer walks first; the truncation is a floor of context, not a loss of
// the record's identity — run id, ticket, final phase and conditions stay).
// It returns the same record for convenience and never drops the fields the
// archive contract keys on (run_id/archive_key/final_phase).
func ApplyArchiveRecordCeilings(rec *castlev1.ArchiveRecord) *castlev1.ArchiveRecord {
	if rec == nil {
		return nil
	}
	rec.SchemaVersion = 1
	rec.RunId = truncate(rec.GetRunId(), maxArchiveRunIDCap)
	rec.ArchiveKey = truncate(rec.GetArchiveKey(), maxArchiveArchiveKeyCap)
	rec.JobName = truncate(rec.GetJobName(), maxArchiveJobNameCap)
	rec.Ticket = truncate(rec.GetTicket(), maxArchiveTicketCap)
	rec.Repo = truncate(rec.GetRepo(), maxArchiveRepoCap)
	rec.WorkflowName = truncate(rec.GetWorkflowName(), maxArchiveWorkflowNameCap)
	rec.WorkflowRef = truncate(rec.GetWorkflowRef(), maxArchiveWorkflowRefCap)
	rec.TreeDigest = truncate(rec.GetTreeDigest(), maxArchiveTreeDigestCap)
	rec.BindingConfigmapChecksum = truncate(rec.GetBindingConfigmapChecksum(), maxArchiveChecksumCap)
	rec.ProjectConfigmapChecksum = truncate(rec.GetProjectConfigmapChecksum(), maxArchiveChecksumCap)
	rec.OperatorVersion = truncate(rec.GetOperatorVersion(), maxArchiveOperatorVerCap)
	rec.FinalPhase = truncate(rec.GetFinalPhase(), maxArchiveFinalPhaseCap)
	rec.FailureReason = truncate(rec.GetFailureReason(), maxArchiveFailureReasonCap)
	rec.StartedAt = truncate(rec.GetStartedAt(), maxArchiveTimestampCap)
	rec.EndedAt = truncate(rec.GetEndedAt(), maxArchiveTimestampCap)
	rec.TerminalCleanup = truncate(rec.GetTerminalCleanup(), maxArchiveTerminalCleanupCap)

	if steps := rec.GetSteps(); len(steps) > MaxArchiveSteps {
		rec.Steps = steps[:MaxArchiveSteps]
	}
	for _, step := range rec.GetSteps() {
		step.Name = truncate(step.GetName(), maxArchiveStepNameCap)
		step.Outcome = truncate(step.GetOutcome(), maxArchiveStepOutcomeCap)
	}

	if conds := rec.GetConditions(); len(conds) > MaxArchiveConditions {
		rec.Conditions = conds[:MaxArchiveConditions]
	}
	for _, cond := range rec.GetConditions() {
		cond.Type = truncate(cond.GetType(), maxArchiveConditionTypeCap)
		cond.Status = truncate(cond.GetStatus(), 16)
		cond.Reason = truncate(cond.GetReason(), maxArchiveCondReasonCap)
		cond.Message = truncate(cond.GetMessage(), maxArchiveConditionMsgCap)
	}
	return rec
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	// byte-truncate deterministically: archive ceilings are evidence caps,
	// not character-perfect editors, and multibyte runes only ever lose a
	// suffix. Truncation is marked with an ellipsis so a reader can tell a
	// capped field apart from short input.
	if max <= len("…") {
		return s[:max]
	}
	return s[:max-len("…")] + "…"
}

// Compile-time proof the client satisfies the controller-facing surface.
var _ RunArchiver = (*RunArchiveClient)(nil)
