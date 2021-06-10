package state

import (
	"reflect"
	"testing"

	"github.com/jmoiron/sqlx"
)

func TestTypingTable(t *testing.T) {
	db, err := sqlx.Open("postgres", postgresConnectionString)
	if err != nil {
		t.Fatalf("failed to open SQL db: %s", err)
	}
	userIDs := []string{
		"@alice:localhost",
		"@bob:localhost",
	}
	roomID := "!foo:localhost"
	table := NewTypingTable(db)
	lastStreamID := -1

	setAndCheck := func() {
		streamID, err := table.SetTyping(roomID, userIDs)
		if err != nil {
			t.Fatalf("failed to SetTyping: %s", err)
		}
		if streamID == 0 {
			t.Errorf("SetTyping: streamID was not returned")
		}
		if lastStreamID >= streamID {
			t.Errorf("SetTyping: streamID returned should always be increasing but it wasn't, got %d, last %d", streamID, lastStreamID)
		}
		lastStreamID = streamID
		gotUserIDs, newStreamID, err := table.Typing(roomID, streamID-1)
		if err != nil {
			t.Fatalf("failed to Typing: %s", err)
		}
		if !reflect.DeepEqual(gotUserIDs, userIDs) {
			t.Errorf("got typing users %v want %v", gotUserIDs, userIDs)
		}
		if newStreamID != streamID {
			t.Errorf("Typing did not return the same stream ID as SetTyping, got %d want %d", newStreamID, streamID)
		}
	}
	setAndCheck()
	userIDs = userIDs[1:]
	userIDs = append(userIDs, "@charlie:localhost")
	setAndCheck()
	userIDs = []string{}
	setAndCheck()
}
