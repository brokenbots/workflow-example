package castle

// Wire-contract tests for the run-archive write leg (KB-314): the exact
// ArchiveRecord body the operator archives, the token lane (never anon),
// idempotence on the effective archive key, the ListRunArchives round-trip,
// and the per-record ceilings castle's unbounded-text store relies on the
// writer to enforce.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/timestamppb"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// archiverHandler mirrors castle's RunArchiveService.ArchiveRun semantics:
// key = archive_key else run_id (both empty, or both set, →
// invalid_argument); a run-id-less record's archive key must carry the
// cr-uid/ prefix; latest wins per key with Created reporting a first write.
type archiverHandler struct {
	castlev1connect.UnimplementedRunArchiveServiceHandler

	mu      sync.Mutex
	records map[string]*castlev1.ArchiveRecord
}

func (h *archiverHandler) effectiveKey(rec *castlev1.ArchiveRecord) (string, error) {
	if rec.GetRunId() != "" && rec.GetArchiveKey() != "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("record may not set both run_id and archive_key"))
	}
	key := rec.GetArchiveKey()
	if key == "" {
		key = rec.GetRunId()
	}
	if key == "" {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("record requires run_id or archive_key"))
	}
	if rec.GetRunId() == "" && !strings.HasPrefix(key, "cr-uid/") {
		return "", connect.NewError(connect.CodeInvalidArgument, errors.New("run-id-less record requires the cr-uid/ archive key namespace"))
	}
	return key, nil
}

func (h *archiverHandler) ArchiveRun(ctx context.Context, req *connect.Request[castlev1.ArchiveRunRequest]) (*connect.Response[castlev1.ArchiveRunResponse], error) {
	key, err := h.effectiveKey(req.Msg.GetRecord())
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	_, seen := h.records[key]
	h.records[key] = req.Msg.GetRecord()
	h.mu.Unlock()
	return connect.NewResponse(&castlev1.ArchiveRunResponse{
		ArchiveId:  RunArchiveID(key),
		ArchivedAt: timestamppb.Now(),
		Created:    !seen,
	}), nil
}

func (h *archiverHandler) ListRunArchives(ctx context.Context, req *connect.Request[castlev1.ListRunArchivesRequest]) (*connect.Response[castlev1.ListRunArchivesResponse], error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	limit := int(req.Msg.GetLimit())
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	// Ascending by effective key, cursor resuming past it (castle's paging).
	keys := make([]string, 0, len(h.records))
	for k := range h.records {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	start := 0
	if cursor := req.Msg.GetCursor(); cursor != "" {
		for start < len(keys) && keys[start] <= cursor {
			start++
		}
	}
	end := start + limit
	if end > len(keys) {
		end = len(keys)
	}
	page := keys[start:end]
	summaries := make([]*castlev1.ArchiveSummary, 0, len(page))
	for _, k := range page {
		summaries = append(summaries, &castlev1.ArchiveSummary{
			RunId:      h.records[k].GetRunId(),
			ArchiveId:  RunArchiveID(k),
			Ticket:     h.records[k].GetTicket(),
			FinalPhase: h.records[k].GetFinalPhase(),
		})
	}
	var next string
	if end < len(keys) {
		next = keys[end]
	}
	return connect.NewResponse(&castlev1.ListRunArchivesResponse{Archives: summaries, NextCursor: next}), nil
}

func (h *archiverHandler) record(key string) *castlev1.ArchiveRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.records[key]
}

func (h *archiverHandler) keyCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.records)
}

// newArchiveServer serves the mirrored handler over h2c (matching the
// production cleartext lane) and returns the stored-record map plus a client
// built the exact production way (nil httpClient).
func newArchiveServer(t *testing.T) (*RunArchiveClient, *archiverHandler) {
	t.Helper()
	handler := &archiverHandler{records: map[string]*castlev1.ArchiveRecord{}}
	path, rpcHandler := castlev1connect.NewRunArchiveServiceHandler(handler)
	mux := http.NewServeMux()
	mux.Handle(path, rpcHandler)
	srv := httptest.NewServer(h2c.NewHandler(mux, &http2.Server{}))
	t.Cleanup(srv.Close)
	return NewRunArchiveClient(RunArchiverConfig{Addr: srv.URL, Token: "operator-token"}, nil), handler
}

func archiveRequest(rec *castlev1.ArchiveRecord) *connect.Request[castlev1.ArchiveRunRequest] {
	return connect.NewRequest(&castlev1.ArchiveRunRequest{Record: rec})
}

// tokenSpy peeks at the outgoing request after the client's transport
// attached its headers but before the wire.
type tokenSpy struct {
	base http.RoundTripper
	seen func(*http.Request)
}

func (s *tokenSpy) RoundTrip(req *http.Request) (*http.Response, error) {
	s.seen(req)
	return s.base.RoundTrip(req)
}

// The archive write rides the criteria token lane (never anonymous) and the
// response's archive id carries the documented run-archive/<key> form.
func TestArchiveRunTokenLaneAndArchiveID(t *testing.T) {
	client, handler := newArchiveServer(t)

	// Peek between the client's token transport and the wire: an assert
	// above it would see the pre-clone request with no headers yet.
	tokenLane := client.httpClient.Transport.(*tokenTransport)
	client.httpClient = &http.Client{Transport: &tokenTransport{
		base: &tokenSpy{
			base: tokenLane.base,
			seen: func(r *http.Request) {
				assert.Equal(t, "Bearer operator-token", r.Header.Get("Authorization"), "the archive lane carries the criteria bearer token")
				assert.Equal(t, "operator-token", r.Header.Get("X-Criteria-Token"))
			},
		},
		token: client.token,
	}}

	resp, err := client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-42", Ticket: "KB-314", FinalPhase: "Succeeded"}))
	require.NoError(t, err)
	assert.Equal(t, RunArchiveID("run-42"), resp.Msg.GetArchiveId())
	assert.True(t, resp.Msg.GetCreated())
	require.NotNil(t, handler.record("run-42"), "run-keyed records store under the run id")
}

// A tokenless archive client fails closed and never wires a byte: the
// refusal is the configured posture (CASTLE_TOKEN is the archive lane's
// auth), not a server PermissionDenied the gate might be tempted to treat
// as a policy decision.
func TestArchiveRunRefusesTokenlessClient(t *testing.T) {
	client, handler := newArchiveServer(t)
	client.token = ""

	_, err := client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-1"}))
	require.Error(t, err)
	assert.ErrorContains(t, err, "CASTLE_TOKEN")
	assert.Equal(t, 0, handler.keyCount(), "no record may be written without a token")
}

// A tokenless client is a fully Disabled lane (R1): the terminal gate
// treats address-without-token like a missing archiver — it skips, terminal
// cleanup proceeds — instead of putting the controller into an unsatisfiable
// requeue loop; the direct ArchiveRun call refuses loudly regardless.
func TestArchiveLaneRequiresAddrAndToken(t *testing.T) {
	addrOnly := NewRunArchiveClient(RunArchiverConfig{Addr: "http://castle:8080", Token: ""}, nil)
	assert.True(t, addrOnly.Disabled(), "address without a token is a disabled archive lane")
	_, err := addrOnly.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-1"}))
	require.Error(t, err)
	assert.ErrorContains(t, err, "CASTLE_TOKEN")

	bothSet := NewRunArchiveClient(RunArchiverConfig{Addr: "http://castle:8080", Token: "tok"}, nil)
	assert.False(t, bothSet.Disabled())
	assert.True(t, (*RunArchiveClient)(nil).Disabled(), "nil receiver stays Disabled")
}

// A disabled archive leg (no castle address) is Disabled and its calls
// refuse; the controller gate skips it instead of retrying forever.
func TestArchiveRunDisabledWithoutAddr(t *testing.T) {
	client := NewRunArchiveClient(RunArchiverConfig{Token: "tok"}, nil)
	assert.True(t, client.Disabled())
	_, err := client.ArchiveRun(context.Background(), archiveRequest(&castlev1.ArchiveRecord{RunId: "run-1"}))
	require.Error(t, err)
	_, err = client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{}))
	require.Error(t, err)
	assert.True(t, (*RunArchiveClient)(nil).Disabled(), "nil receiver stays Disabled")
}

// Re-archiving the same effective key is idempotent (latest wins, Created
// false on the update), synthetic cr-uid keys are accepted with run_id
// empty, and ListRunArchives pages back exactly what was stored.
func TestArchiveRunIdempotentAndListed(t *testing.T) {
	client, handler := newArchiveServer(t)

	resp, err := client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-7", Ticket: "T7", FinalPhase: "Succeeded"}))
	require.NoError(t, err)
	assert.True(t, resp.Msg.GetCreated())

	resp, err = client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-7", Ticket: "T7", FinalPhase: "Failed", FailureReason: "late verdict"}))
	require.NoError(t, err)
	assert.False(t, resp.Msg.GetCreated(), "a second archive of the same key is an update")
	assert.Equal(t, RunArchiveID("run-7"), resp.Msg.GetArchiveId())
	assert.Equal(t, "late verdict", handler.record("run-7").GetFailureReason())

	resp, err = client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{ArchiveKey: CRUIDArchiveKey("uid-9"), Ticket: "T9", FinalPhase: "Failed"}))
	require.NoError(t, err)
	assert.Equal(t, RunArchiveID("cr-uid/uid-9"), resp.Msg.GetArchiveId())

	// The backfill's coverage walk: prefix-filtered paging with a limit of
	// one exercises every cursor hop.
	listed := map[string]bool{}
	cursor := ""
	for {
		page, err := client.ListRunArchives(context.Background(), connect.NewRequest(
			&castlev1.ListRunArchivesRequest{KeyPrefix: "cr-uid/", Limit: 1, Cursor: cursor}))
		require.NoError(t, err)
		for _, sum := range page.Msg.GetArchives() {
			listed[sum.GetArchiveId()] = true
		}
		cursor = page.Msg.GetNextCursor()
		if cursor == "" {
			break
		}
	}
	assert.True(t, listed[RunArchiveID("cr-uid/uid-9")])
	assert.False(t, listed[RunArchiveID("run-7")], "a cr-uid page walk must not reach run-keyed records")
}

// Castle's record keying rules surface to the operator gate: a record
// carrying both idents (or a bare non-cr-uid archive_key) is
// invalid_argument — the operator's fallback clears run_id before it
// switches to the cr-uid key.
func TestArchiveRunRejectsAmbiguousKeying(t *testing.T) {
	client, handler := newArchiveServer(t)

	_, err := client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{RunId: "run-1", ArchiveKey: CRUIDArchiveKey("uid-1")}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))

	_, err = client.ArchiveRun(context.Background(),
		archiveRequest(&castlev1.ArchiveRecord{ArchiveKey: "run-1", Ticket: "T1"}))
	require.Error(t, err)
	assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err), "a bare archive_key outside the cr-uid/ namespace is invalid")
	assert.Equal(t, 0, handler.keyCount())
}

// The per-record ceilings: oversized step/condition lists are capped with
// their FIRST entries kept, free-form fields are truncated with a marker,
// and the fields the contract keys on (run_id/archive_key/final_phase) are
// never dropped.
func TestApplyArchiveRecordCeilings(t *testing.T) {
	rec := ApplyArchiveRecordCeilings(bigTestArchiveRecord())
	require.Equal(t, int32(1), rec.GetSchemaVersion())
	require.Len(t, rec.GetSteps(), MaxArchiveSteps)
	assert.Equal(t, "step-000", rec.GetSteps()[0].GetName(), "first entries kept on cap")
	assert.Len(t, rec.GetConditions(), MaxArchiveConditions)
	assert.Equal(t, "Cond-0", rec.GetConditions()[0].GetType())

	longRec := ApplyArchiveRecordCeilings(&castlev1.ArchiveRecord{
		Conditions: []*castlev1.ArchiveCondition{{Message: strings.Repeat("x", maxArchiveConditionMsgCap*2)}},
	})
	assert.Len(t, longRec.GetConditions()[0].GetMessage(), maxArchiveConditionMsgCap)
	assert.True(t, strings.HasSuffix(longRec.GetConditions()[0].GetMessage(), "…"), "truncation is marked")

	kept := ApplyArchiveRecordCeilings(&castlev1.ArchiveRecord{
		RunId: "run-9", ArchiveKey: CRUIDArchiveKey("uid-9"), FinalPhase: "Failed",
		Steps: []*castlev1.ArchiveStep{{Name: "keep-me"}},
	})
	assert.Equal(t, "run-9", kept.GetRunId())
	assert.Equal(t, CRUIDArchiveKey("uid-9"), kept.GetArchiveKey())
	assert.Equal(t, "Failed", kept.GetFinalPhase())
	require.Len(t, kept.GetSteps(), 1)
	assert.Equal(t, "keep-me", kept.GetSteps()[0].GetName())

	assert.Nil(t, ApplyArchiveRecordCeilings(nil))
}

// A cap smaller than the ellipsis marker byte-truncates without emitting
// the marker: tiny caps still keep the field's prefix (never dropping it
// entirely), and cuts never split a UTF-8 rune.
func TestTruncateTinyCap(t *testing.T) {
	assert.Equal(t, "ab", truncate("abcdef", 2))
	assert.Equal(t, "abcdef", truncate("abcdef", 10))
	assert.Equal(t, "", truncate("", 5))
	// Rune-boundary regression: the byte cap must not land inside the
	// multibyte rune ("a" + four two-byte é = 11 bytes; cap 6 → marker
	// fits, cut backs up off the rune's continuation byte).
	assert.Equal(t, "aé…", truncate("aéééé", 6))
	// Marker-sized cut inside a rune with no room to back up cleanly
	// under the marker: the rune-safe prefix is kept, unmarked.
	assert.Equal(t, "aé", truncate("aéé", 3))
}

func bigTestArchiveRecord() *castlev1.ArchiveRecord {
	steps := make([]*castlev1.ArchiveStep, 0, MaxArchiveSteps+2)
	for i := range MaxArchiveSteps + 2 {
		name := "step-0"
		if i == 0 {
			name = "step-000"
		}
		steps = append(steps, &castlev1.ArchiveStep{Name: name})
	}
	conds := make([]*castlev1.ArchiveCondition, 0, MaxArchiveConditions+3)
	for i := range MaxArchiveConditions + 3 {
		typ := "Cond-1"
		if i == 0 {
			typ = "Cond-0"
		}
		conds = append(conds, &castlev1.ArchiveCondition{Type: typ})
	}
	return &castlev1.ArchiveRecord{RunId: "run-big", Steps: steps, Conditions: conds}
}
