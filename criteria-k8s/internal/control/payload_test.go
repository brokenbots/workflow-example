package control

// payload_test.go pins the DispatchEnvelope wire contract (KB-338): the
// shared writer (adapter) and reader (operator agent) must round-trip, and
// everything un-attributable must fail closed.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const fixtureEnvelopeJSON = `{"kind":"criteria-workflow-assignment/1","ticket":"KB-338",` +
	`"repo":"https://github.com/brokenbots/workflow-example.git","class":"default",` +
	`"slot":"repo-config","targetImages":{"shell":"localhost:5000/shell:pinned"}}`

func TestMarshalDispatchEnvelopeSetsKindAndRoundTrips(t *testing.T) {
	data, err := MarshalDispatchEnvelope(DispatchEnvelope{
		Ticket: "KB-338",
		Repo:   "https://github.com/brokenbots/workflow-example.git",
		Class:  "default",
		Slot:   "repo-config",
		TargetImages: map[string]string{
			"shell": "localhost:5000/shell:pinned",
		},
	})
	require.NoError(t, err)
	decoded, err := ParseDispatchEnvelope(data)
	require.NoError(t, err)
	assert.Equal(t, DispatchEnvelopeKind, decoded.Kind)
	assert.Equal(t, "KB-338", decoded.Ticket)
	assert.Equal(t, "https://github.com/brokenbots/workflow-example.git", decoded.Repo)
	assert.Equal(t, "default", decoded.Class)
	assert.Equal(t, "repo-config", decoded.Slot)
	assert.Equal(t, map[string]string{"shell": "localhost:5000/shell:pinned"}, decoded.TargetImages)
}

func TestParseDispatchEnvelopeIsTheFixtureContract(t *testing.T) {
	decoded, err := ParseDispatchEnvelope(fixtureEnvelopeJSON)
	require.NoError(t, err)
	assert.Equal(t, "KB-338", decoded.Ticket)
	assert.Equal(t, "repo-config", decoded.Slot)
	assert.Equal(t, "localhost:5000/shell:pinned", decoded.TargetImages["shell"])
}

func TestParseDispatchEnvelopeFailClosed(t *testing.T) {
	cases := map[string]struct {
		source    string
		expectHas string
	}{
		"empty source":        {"", "empty"},
		"plain text":          {"some workflow source hcl", "not a dispatch payload"},
		"json other shape":    {`{"foo":1}`, "unknown kind"},
		"unknown kind":        {`{"kind":"criteria-workflow-assignment/2","ticket":"KB-1","repo":"https://x"}`, "unknown kind"},
		"empty ticket":        {`{"kind":"criteria-workflow-assignment/1","repo":"https://x"}`, "ticket empty"},
		"empty repo":          {`{"kind":"criteria-workflow-assignment/1","ticket":"KB-1"}`, "repo empty"},
		"empty target kind":   {`{"kind":"criteria-workflow-assignment/1","ticket":"KB-1","repo":"https://x","targetImages":{"":"img"}}`, "empty adapter kind"},
		"empty target image":  {`{"kind":"criteria-workflow-assignment/1","ticket":"KB-1","repo":"https://x","targetImages":{"shell":""}}`, "empty reference"},
		"truncated json":      {`{"kind":"criteria-workflow-assignment/1","ticket":`, "not a dispatch payload"},
		"wrong primitive top": {`"workflow source"`, "not a dispatch payload"},
	}
	for name, tc := range cases {
		_, err := ParseDispatchEnvelope(tc.source)
		require.Error(t, err, "case %q must fail closed", name)
		assert.True(t, strings.Contains(err.Error(), tc.expectHas),
			"case %q: error %q must mention %q", name, err.Error(), tc.expectHas)
	}
}

func TestMarshalDispatchEnvelopeRejectsInvalidPayload(t *testing.T) {
	_, err := MarshalDispatchEnvelope(DispatchEnvelope{Ticket: "KB-1"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "repo empty")
}