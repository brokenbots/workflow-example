package events

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOutcome(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"run.started"}`,
		`{"type":"pr.created","pr_number":42}`,
		`{"type":"ticket.updated","state":"Done"}`,
		`{"type":"run.completed"}`,
	}, "\n")

	outcome, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "42", outcome.PRNumber)
	assert.Equal(t, "Done", outcome.TicketState)
}

func TestParseOutcomeMissingFields(t *testing.T) {
	input := `{"type":"run.completed"}`
	outcome, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "", outcome.PRNumber)
	assert.Equal(t, "", outcome.TicketState)
}

func TestParseOutcomeFromURL(t *testing.T) {
	input := `{"message":"opened https://github.com/brokenbots/workflow-example/pull/7"}`
	outcome, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "7", outcome.PRNumber)
}

func TestParseOutcomeIgnoresInvalidJSON(t *testing.T) {
	input := `not json`
	outcome, err := ParseBytes([]byte(input))
	require.NoError(t, err)
	assert.Equal(t, "", outcome.PRNumber)
}
