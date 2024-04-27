package state

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/sliding-sync/sync2"

	"github.com/jmoiron/sqlx"
	"github.com/matrix-org/gomatrixserverlib/spec"
	"github.com/matrix-org/sliding-sync/internal"
	"github.com/matrix-org/sliding-sync/sqlutil"
	"github.com/matrix-org/sliding-sync/testutils"
	"github.com/tidwall/gjson"
)

func TestStorageRoomStateBeforeAndAfterEventPosition(t *testing.T) {
	ctx := context.Background()
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()
	roomID := "!TestStorageRoomStateAfterEventPosition:localhost"
	alice := "@alice:localhost"
	bob := "@bob:localhost"
	events := []json.RawMessage{
		testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{"creator": alice}),
		testutils.NewJoinEvent(t, alice),
		testutils.NewStateEvent(t, "m.room.join_rules", "", alice, map[string]interface{}{"join_rule": "invite"}),
		testutils.NewStateEvent(t, "m.room.member", bob, alice, map[string]interface{}{"membership": "invite"}),
	}
	accResult, err := store.Accumulate(userID, roomID, sync2.TimelineResponse{Events: events})
	if err != nil {
		t.Fatalf("Accumulate returned error: %s", err)
	}
	latest := accResult.TimelineNIDs[len(accResult.TimelineNIDs)-1]

	testCases := []struct {
		name       string
		getEvents  func() []Event
		wantEvents []json.RawMessage
	}{
		{
			name: "room state after the latest position includes the invite event",
			getEvents: func() []Event {
				events, err := store.RoomStateAfterEventPosition(ctx, []string{roomID}, latest, nil)
				if err != nil {
					t.Fatalf("RoomStateAfterEventPosition: %s", err)
				}
				return events[roomID]
			},
			wantEvents: events[:],
		},
		{
			name: "room state after the latest position filtered for join_rule returns a single event",
			getEvents: func() []Event {
				events, err := store.RoomStateAfterEventPosition(ctx, []string{roomID}, latest, map[string][]string{"m.room.join_rules": nil})
				if err != nil {
					t.Fatalf("RoomStateAfterEventPosition: %s", err)
				}
				return events[roomID]
			},
			wantEvents: []json.RawMessage{
				events[2],
			},
		},
		{
			name: "room state after the latest position filtered for join_rule and create event excludes member events",
			getEvents: func() []Event {
				events, err := store.RoomStateAfterEventPosition(ctx, []string{roomID}, latest, map[string][]string{
					"m.room.join_rules": []string{""},
					"m.room.create":     nil, // all matching state events with this event type
				})
				if err != nil {
					t.Fatalf("RoomStateAfterEventPosition: %s", err)
				}
				return events[roomID]
			},
			wantEvents: []json.RawMessage{
				events[0], events[2],
			},
		},
		{
			name: "room state after the latest position filtered for all members returns all member events",
			getEvents: func() []Event {
				events, err := store.RoomStateAfterEventPosition(ctx, []string{roomID}, latest, map[string][]string{
					"m.room.member": nil, // all matching state events with this event type
				})
				if err != nil {
					t.Fatalf("RoomStateAfterEventPosition: %s", err)
				}
				return events[roomID]
			},
			wantEvents: []json.RawMessage{
				events[1], events[3],
			},
		},
	}

	for _, tc := range testCases {
		gotEvents := tc.getEvents()
		if len(gotEvents) != len(tc.wantEvents) {
			t.Errorf("%s: got %d events want %d : got %+v", tc.name, len(gotEvents), len(tc.wantEvents), gotEvents)
			continue
		}
		for i, eventJSON := range tc.wantEvents {
			if !bytes.Equal(eventJSON, gotEvents[i].JSON) {
				t.Errorf("%s: pos %d\ngot  %s\nwant %s", tc.name, i, string(gotEvents[i].JSON), string(eventJSON))
			}
		}
	}
}

func TestStorageJoinedRoomsAfterPosition(t *testing.T) {
	// Clean DB. If we don't, other tests' events will be in the DB, but we won't
	// provide keys in the metadata dict we pass to MetadataForAllRooms, leading to a
	// panic.
	if err := cleanDB(t); err != nil {
		t.Fatalf("failed to wipe DB: %s", err)
	}
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()
	joinedRoomID := "!joined:bar"
	invitedRoomID := "!invited:bar"
	leftRoomID := "!left:bar"
	banRoomID := "!ban:bar"
	bobJoinedRoomID := "!bobjoined:bar"
	alice := "@aliceTestStorageJoinedRoomsAfterPosition:localhost"
	bob := "@bobTestStorageJoinedRoomsAfterPosition:localhost"
	charlie := "@charlieTestStorageJoinedRoomsAfterPosition:localhost"
	roomIDToEventMap := map[string][]json.RawMessage{
		joinedRoomID: {
			testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{"creator": alice}),
			testutils.NewJoinEvent(t, alice),
		},
		invitedRoomID: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewStateEvent(t, "m.room.member", alice, bob, map[string]interface{}{"membership": "invite"}),
		},
		leftRoomID: {
			testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{"creator": alice}),
			testutils.NewJoinEvent(t, alice),
			testutils.NewStateEvent(t, "m.room.member", alice, alice, map[string]interface{}{"membership": "leave"}),
		},
		banRoomID: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewJoinEvent(t, alice),
			testutils.NewStateEvent(t, "m.room.member", alice, bob, map[string]interface{}{"membership": "ban"}),
		},
		bobJoinedRoomID: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewJoinEvent(t, charlie),
		},
	}
	var latestPos int64
	var err error
	for roomID, eventMap := range roomIDToEventMap {
		accResult, err := store.Accumulate(userID, roomID, sync2.TimelineResponse{Events: eventMap})
		if err != nil {
			t.Fatalf("Accumulate on %s failed: %s", roomID, err)
		}
		latestPos = accResult.TimelineNIDs[len(accResult.TimelineNIDs)-1]
	}
	aliceJoinTimingsByRoomID, err := store.JoinedRoomsAfterPosition(alice, latestPos)
	if err != nil {
		t.Fatalf("failed to JoinedRoomsAfterPosition: %s", err)
	}
	if len(aliceJoinTimingsByRoomID) != 1 {
		t.Fatalf("JoinedRoomsAfterPosition at %v for %s got %v, want room %s only", latestPos, alice, aliceJoinTimingsByRoomID, joinedRoomID)
	}
	for gotRoomID, _ := range aliceJoinTimingsByRoomID {
		if gotRoomID != joinedRoomID {
			t.Fatalf("JoinedRoomsAfterPosition at %v for %s got %v want %v", latestPos, alice, gotRoomID, joinedRoomID)
		}
	}
	bobJoinTimingsByRoomID, err := store.JoinedRoomsAfterPosition(bob, latestPos)
	if err != nil {
		t.Fatalf("failed to JoinedRoomsAfterPosition: %s", err)
	}
	if len(bobJoinTimingsByRoomID) != 3 {
		t.Fatalf("JoinedRoomsAfterPosition for %s got %v rooms want %v", bob, len(bobJoinTimingsByRoomID), 3)
	}

	// also test currentNotMembershipStateEventsInAllRooms
	txn := store.DB.MustBeginTx(context.Background(), nil)
	roomIDToCreateEvents, err := store.currentNotMembershipStateEventsInAllRooms(txn, []string{"m.room.create"})
	if err != nil {
		t.Fatalf("CurrentStateEventsInAllRooms returned error: %s", err)
	}
	for roomID := range roomIDToEventMap {
		if _, ok := roomIDToCreateEvents[roomID]; !ok {
			t.Fatalf("CurrentStateEventsInAllRooms missed room ID %s", roomID)
		}
	}
	for roomID := range roomIDToEventMap {
		createEvents := roomIDToCreateEvents[roomID]
		if createEvents == nil {
			t.Errorf("CurrentStateEventsInAllRooms: unknown room %v", roomID)
		}
		if len(createEvents) != 1 {
			t.Fatalf("CurrentStateEventsInAllRooms got %d events, want 1", len(createEvents))
		}
		if len(createEvents[0].JSON) < 20 { // make sure there's something here
			t.Errorf("CurrentStateEventsInAllRooms: got wrong json for event, got %s", string(createEvents[0].JSON))
		}
	}

	newMetadata := func(roomID string, joinCount, inviteCount int) internal.RoomMetadata {
		m := internal.NewRoomMetadata(roomID)
		m.JoinCount = joinCount
		m.InviteCount = inviteCount
		return *m
	}

	// also test MetadataForAllRooms
	roomIDToMetadata := map[string]internal.RoomMetadata{
		joinedRoomID:    newMetadata(joinedRoomID, 1, 0),
		invitedRoomID:   newMetadata(invitedRoomID, 1, 1),
		banRoomID:       newMetadata(banRoomID, 1, 0),
		bobJoinedRoomID: newMetadata(bobJoinedRoomID, 2, 0),
	}

	tempTableName, err := store.PrepareSnapshot(txn)
	if err != nil {
		t.Fatalf("PrepareSnapshot: %s", err)
	}
	err = store.MetadataForAllRooms(txn, tempTableName, roomIDToMetadata)
	txn.Commit()
	if err != nil {
		t.Fatalf("MetadataForAllRooms: %s", err)
	}
	wantHeroInfos := map[string]internal.RoomMetadata{
		joinedRoomID: {
			JoinCount: 1,
		},
		invitedRoomID: {
			JoinCount:   1,
			InviteCount: 1,
		},
		banRoomID: {
			JoinCount: 1,
		},
		bobJoinedRoomID: {
			JoinCount: 2,
		},
	}
	for roomID, wantHI := range wantHeroInfos {
		gotHI := roomIDToMetadata[roomID]
		if gotHI.InviteCount != wantHI.InviteCount {
			t.Errorf("hero info for %s got %d invited users, want %d", roomID, gotHI.InviteCount, wantHI.InviteCount)
		}
		if gotHI.JoinCount != wantHI.JoinCount {
			t.Errorf("hero info for %s got %d joined users, want %d", roomID, gotHI.JoinCount, wantHI.JoinCount)
		}
	}
}

// Test the examples on VisibleEventNIDsBetween docs
func TestVisibleEventNIDsBetween(t *testing.T) {
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()
	roomA := "!a:localhost"
	roomB := "!b:localhost"
	roomC := "!c:localhost"
	roomD := "!d:localhost"
	roomE := "!e:localhost"
	alice := "@alice_TestVisibleEventNIDsBetween:localhost"
	bob := "@bob_TestVisibleEventNIDsBetween:localhost"

	// bob makes all these rooms first, alice is already joined to C
	roomIDToEventMap := map[string][]json.RawMessage{
		roomA: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
		},
		roomB: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
		},
		roomC: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewJoinEvent(t, alice),
		},
		roomD: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
		},
		roomE: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
		},
	}
	for roomID, eventMap := range roomIDToEventMap {
		_, err := store.Initialise(roomID, eventMap)
		if err != nil {
			t.Fatalf("Initialise on %s failed: %s", roomID, err)
		}
	}
	startPos, err := store.LatestEventNID()
	if err != nil {
		t.Fatalf("LatestEventNID: %s", err)
	}

	baseTimestamp := spec.Timestamp(1632131678061).Time()
	// Test the examples
	//                     Stream Positions
	//           1     2   3    4   5   6   7   8   9   10
	//   Room A  Maj   E   E                E
	//   Room B                 E   Maj E
	//   Room C                                 E   Mal E   (a already joined to this room)
	timelineInjections := []struct {
		RoomID string
		Events []json.RawMessage
	}{
		{
			RoomID: roomA,
			Events: []json.RawMessage{
				testutils.NewJoinEvent(t, alice),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
			},
		},
		{
			RoomID: roomB,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewJoinEvent(t, alice),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
			},
		},
		{
			RoomID: roomA,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
			},
		},
		{
			RoomID: roomC,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewStateEvent(t, "m.room.member", alice, alice, map[string]interface{}{"membership": "leave"}),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
			},
		},
	}
	for _, tl := range timelineInjections {
		accResult, err := store.Accumulate(userID, tl.RoomID, sync2.TimelineResponse{Events: tl.Events})
		if err != nil {
			t.Fatalf("Accumulate on %s failed: %s", tl.RoomID, err)
		}
		t.Logf("%s added %d new events", tl.RoomID, accResult.NumNew)
	}
	latestPos, err := store.LatestEventNID()
	if err != nil {
		t.Fatalf("LatestEventNID: %s", err)
	}
	t.Logf("ABC Start=%d Latest=%d", startPos, latestPos)
	roomIDToVisibleRange, err := store.VisibleEventNIDsBetween(alice, startPos, latestPos)
	if err != nil {
		t.Fatalf("VisibleEventNIDsBetween to %d: %s", latestPos, err)
	}
	for roomID, r := range roomIDToVisibleRange {
		t.Logf("%v => [%d,%d]", roomID, r[0]-startPos, r[1]-startPos)
	}
	if len(roomIDToVisibleRange) != 3 {
		t.Errorf("VisibleEventNIDsBetween: wrong number of rooms, want 3 got %+v", roomIDToVisibleRange)
	}

	// check that we can query subsets too
	roomIDToVisibleRangesSubset, err := store.visibleEventNIDsBetweenForRooms(alice, []string{roomA, roomB}, startPos, latestPos)
	if err != nil {
		t.Fatalf("VisibleEventNIDsBetweenForRooms to %d: %s", latestPos, err)
	}
	if len(roomIDToVisibleRangesSubset) != 2 {
		t.Errorf("VisibleEventNIDsBetweenForRooms: wrong number of rooms, want 2 got %+v", roomIDToVisibleRange)
	}

	// For Room A: from=1, to=10, returns { RoomA: [ [1,10] ]}  (tests events in joined room)
	verifyRange(t, roomIDToVisibleRange, roomA, [2]int64{
		1 + startPos, 10 + startPos,
	})

	// For Room B: from=1, to=10, returns { RoomB: [ [5,10] ]}  (tests joining a room starts events)
	verifyRange(t, roomIDToVisibleRange, roomB, [2]int64{
		5 + startPos, 10 + startPos,
	})

	// For Room C: from=1, to=10, returns { RoomC: [ [0,9] ]}  (tests leaving a room stops events)
	// We start at 0 because it's the earliest event (we were joined since the beginning of the room state)
	verifyRange(t, roomIDToVisibleRange, roomC, [2]int64{
		0 + startPos, 9 + startPos,
	})

	// change the users else we will still have some rooms from A,B,C present if the user is still joined
	// to those rooms.
	alice = "@aliceDE:localhost"
	bob = "@bobDE:localhost"

	//                     Stream Positions
	//           1     2   3    4   5   6   7   8   9   10  11  12  13  14  15
	//   Room D  Maj                E   Mal E   Maj E   Mal E
	//   Room E        E   Mai  E                               E   Maj E   E
	timelineInjections = []struct {
		RoomID string
		Events []json.RawMessage
	}{
		{
			RoomID: roomD,
			Events: []json.RawMessage{
				testutils.NewJoinEvent(t, alice),
			},
		},
		{
			RoomID: roomE,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewStateEvent(t, "m.room.member", alice, bob, map[string]interface{}{"membership": "invite"}),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp.Add(1*time.Second))),
			},
		},
		{
			RoomID: roomD,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewStateEvent(t, "m.room.member", alice, alice, map[string]interface{}{"membership": "leave"}),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewJoinEvent(t, alice),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp.Add(1*time.Second))),
				testutils.NewStateEvent(t, "m.room.member", alice, alice, map[string]interface{}{"membership": "leave"}),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp.Add(1*time.Second))),
			},
		},
		{
			RoomID: roomE,
			Events: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewJoinEvent(t, alice),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
				testutils.NewEvent(t, "m.room.message", bob, map[string]interface{}{}, testutils.WithTimestamp(baseTimestamp)),
			},
		},
	}
	startPos, err = store.LatestEventNID()
	if err != nil {
		t.Fatalf("LatestEventNID: %s", err)
	}
	for _, tl := range timelineInjections {
		accResult, err := store.Accumulate(userID, tl.RoomID, sync2.TimelineResponse{Events: tl.Events})
		if err != nil {
			t.Fatalf("Accumulate on %s failed: %s", tl.RoomID, err)
		}
		t.Logf("%s added %d new events", tl.RoomID, accResult.NumNew)
	}
	latestPos, err = store.LatestEventNID()
	if err != nil {
		t.Fatalf("LatestEventNID: %s", err)
	}
	t.Logf("DE Start=%d Latest=%d", startPos, latestPos)
	roomIDToVisibleRange, err = store.VisibleEventNIDsBetween(alice, startPos, latestPos)
	if err != nil {
		t.Fatalf("VisibleEventNIDsBetween to %d: %s", latestPos, err)
	}
	for roomID, r := range roomIDToVisibleRange {
		t.Logf("%v => [%d,%d]", roomID, r[0]-startPos, r[1]-startPos)
	}
	if len(roomIDToVisibleRange) != 2 {
		t.Errorf("VisibleEventNIDsBetween: wrong number of rooms, want 2 got %+v", roomIDToVisibleRange)
	}

	// For Room D: from=1, to=15 returns { RoomD: [ 8,10 ] } (tests multi-join/leave)
	verifyRange(t, roomIDToVisibleRange, roomD, [2]int64{
		8 + startPos, 10 + startPos,
	})

	// For Room E: from=1, to=15 returns { RoomE: [ 13,15 ] } (tests invites)
	verifyRange(t, roomIDToVisibleRange, roomE, [2]int64{
		13 + startPos, 15 + startPos,
	})

}

func TestStorageLatestEventsInRoomsPrevBatch(t *testing.T) {
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()
	roomID := "!joined:bar"
	alice := "@alice_TestStorageLatestEventsInRoomsPrevBatch:localhost"
	stateEvents := []json.RawMessage{
		testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{"creator": alice}),
		testutils.NewJoinEvent(t, alice),
	}
	timelines := []struct {
		timeline  []json.RawMessage
		prevBatch string
	}{
		{
			prevBatch: "batch A",
			timeline: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "1"}), // prev batch should be associated with this event
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "2"}),
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "3"}),
			},
		},
		{
			prevBatch: "batch B",
			timeline: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "4"}),
			},
		},
		{
			prevBatch: "batch C",
			timeline: []json.RawMessage{
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "5"}),
				testutils.NewEvent(t, "m.room.message", alice, map[string]interface{}{"body": "6"}),
			},
		},
	}

	_, err := store.Initialise(roomID, stateEvents)
	if err != nil {
		t.Fatalf("failed to initialise: %s", err)
	}
	eventIDs := []string{}
	for _, timeline := range timelines {
		_, err = store.Accumulate(userID, roomID, sync2.TimelineResponse{Events: timeline.timeline, PrevBatch: timeline.prevBatch})
		if err != nil {
			t.Fatalf("failed to accumulate: %s", err)
		}
		for _, ev := range timeline.timeline {
			eventIDs = append(eventIDs, gjson.ParseBytes(ev).Get("event_id").Str)
		}
	}
	t.Logf("events: %v", eventIDs)
	var idsToNIDs map[string]int64
	sqlutil.WithTransaction(store.EventsTable.db, func(txn *sqlx.Tx) error {
		idsToNIDs, err = store.EventsTable.SelectNIDsByIDs(txn, eventIDs)
		if err != nil {
			t.Fatalf("failed to get nids for events: %s", err)
		}
		return nil
	})
	t.Logf("nids: %v", idsToNIDs)
	wantPrevBatches := []string{
		// first chunk
		timelines[0].prevBatch,
		timelines[1].prevBatch,
		timelines[1].prevBatch,
		// second chunk
		timelines[1].prevBatch,
		// third chunk
		timelines[2].prevBatch,
		"",
	}

	for i := range wantPrevBatches {
		wantPrevBatch := wantPrevBatches[i]
		eventNID := idsToNIDs[eventIDs[i]]
		// closest batch to the last event in the chunk (latest nid) is always the next prev batch token
		var pb string
		_ = sqlutil.WithTransaction(store.DB, func(txn *sqlx.Tx) (err error) {
			pb, err = store.EventsTable.SelectClosestPrevBatch(txn, roomID, eventNID)
			if err != nil {
				t.Fatalf("failed to SelectClosestPrevBatch: %s", err)
			}
			return nil
		})

		if pb != wantPrevBatch {
			t.Fatalf("SelectClosestPrevBatch: got %v want %v", pb, wantPrevBatch)
		}
	}
}

func TestGlobalSnapshot(t *testing.T) {
	alice := "@TestGlobalSnapshot_alice:localhost"
	bob := "@TestGlobalSnapshot_bob:localhost"
	roomAlice := "!alice"
	roomBob := "!bob"
	roomAliceBob := "!alicebob"
	roomSpace := "!space"
	oldRoomID := "!old"
	newRoomID := "!new"
	roomType := "room_type_here"
	spaceRoomType := "m.space"
	roomIDToEventMap := map[string][]json.RawMessage{
		roomAlice: {
			testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{"creator": alice, "predecessor": map[string]string{
				"room_id":  oldRoomID,
				"event_id": "$something",
			}}),
			testutils.NewJoinEvent(t, alice),
			testutils.NewStateEvent(t, "m.room.encryption", "", alice, map[string]interface{}{"algorithm": "m.megolm.v1.aes-sha2"}),
		},
		roomBob: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob, "type": roomType}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewStateEvent(t, "m.room.name", "", alice, map[string]interface{}{"name": "My Room"}),
		},
		roomAliceBob: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewJoinEvent(t, alice),
			testutils.NewStateEvent(t, "m.room.canonical_alias", "", alice, map[string]interface{}{"alias": "#alias"}),
			testutils.NewStateEvent(t, "m.room.tombstone", "", alice, map[string]interface{}{"replacement_room": newRoomID, "body": "yep"}),
		},
		roomSpace: {
			testutils.NewStateEvent(t, "m.room.create", "", bob, map[string]interface{}{"creator": bob, "type": spaceRoomType}),
			testutils.NewJoinEvent(t, bob),
			testutils.NewStateEvent(t, "m.space.child", newRoomID, bob, map[string]interface{}{"via": []string{"somewhere"}}),
			testutils.NewStateEvent(t, "m.space.child", "!no_via", bob, map[string]interface{}{}),
			testutils.NewStateEvent(t, "m.room.member", alice, bob, map[string]interface{}{"membership": "invite"}),
		},
	}
	if err := cleanDB(t); err != nil {
		t.Fatalf("failed to wipe DB: %s", err)
	}

	store := NewStorage(postgresConnectionString)
	defer store.Teardown()
	for roomID, stateEvents := range roomIDToEventMap {
		_, err := store.Initialise(roomID, stateEvents)
		assertNoError(t, err)
	}
	snapshot, err := store.GlobalSnapshot()
	assertNoError(t, err)
	wantJoinedMembers := map[string][]string{
		roomAlice:    {alice},
		roomBob:      {bob},
		roomAliceBob: {bob, alice}, // user IDs are ordered by event nid, and bob joined first so he is first
		roomSpace:    {bob},
	}
	if !reflect.DeepEqual(snapshot.AllJoinedMembers, wantJoinedMembers) {
		t.Errorf("Snapshot.AllJoinedMembers:\ngot:  %+v\nwant: %+v", snapshot.AllJoinedMembers, wantJoinedMembers)
	}
	wantMetadata := map[string]internal.RoomMetadata{
		roomAlice: {
			RoomID:               roomAlice,
			JoinCount:            1,
			LastMessageTimestamp: gjson.ParseBytes(roomIDToEventMap[roomAlice][len(roomIDToEventMap[roomAlice])-1]).Get("origin_server_ts").Uint(),
			Heroes:               []internal.Hero{{ID: alice}},
			Encrypted:            true,
			PredecessorRoomID:    &oldRoomID,
			ChildSpaceRooms:      make(map[string]struct{}),
		},
		roomBob: {
			RoomID:               roomBob,
			JoinCount:            1,
			LastMessageTimestamp: gjson.ParseBytes(roomIDToEventMap[roomBob][len(roomIDToEventMap[roomBob])-1]).Get("origin_server_ts").Uint(),
			Heroes:               []internal.Hero{{ID: bob}},
			NameEvent:            "My Room",
			RoomType:             &roomType,
			ChildSpaceRooms:      make(map[string]struct{}),
		},
		roomAliceBob: {
			RoomID:               roomAliceBob,
			JoinCount:            2,
			LastMessageTimestamp: gjson.ParseBytes(roomIDToEventMap[roomAliceBob][len(roomIDToEventMap[roomAliceBob])-1]).Get("origin_server_ts").Uint(),
			Heroes:               []internal.Hero{{ID: bob}, {ID: alice}},
			CanonicalAlias:       "#alias",
			UpgradedRoomID:       &newRoomID,
			ChildSpaceRooms:      make(map[string]struct{}),
		},
		roomSpace: {
			RoomID:               roomSpace,
			JoinCount:            1,
			InviteCount:          1,
			LastMessageTimestamp: gjson.ParseBytes(roomIDToEventMap[roomSpace][len(roomIDToEventMap[roomSpace])-1]).Get("origin_server_ts").Uint(),
			Heroes:               []internal.Hero{{ID: bob}, {ID: alice}},
			RoomType:             &spaceRoomType,
			ChildSpaceRooms: map[string]struct{}{
				newRoomID: {},
			},
		},
	}
	for roomID, want := range wantMetadata {
		assertRoomMetadata(t, snapshot.GlobalMetadata[roomID], want)
	}
}

func TestAllJoinedMembers(t *testing.T) {
	assertNoError(t, cleanDB(t))
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()

	alice := "@alice:localhost"
	bob := "@bob:localhost"
	charlie := "@charlie:localhost"
	doris := "@doris:localhost"
	eve := "@eve:localhost"
	frank := "@frank:localhost"

	// Alice is always the creator and the inviter for simplicity's sake
	testCases := []struct {
		Name                  string
		InitMemberships       [][2]string
		AccumulateMemberships [][2]string
		RoomID                string // tests set this dynamically
		WantJoined            []string
		WantInvited           []string
	}{
		{
			Name:                  "basic joined users",
			InitMemberships:       [][2]string{{alice, "join"}},
			AccumulateMemberships: [][2]string{{bob, "join"}},
			WantJoined:            []string{alice, bob},
		},
		{
			Name:                  "basic invited users",
			InitMemberships:       [][2]string{{alice, "join"}, {charlie, "invite"}},
			AccumulateMemberships: [][2]string{{bob, "invite"}},
			WantJoined:            []string{alice},
			WantInvited:           []string{bob, charlie},
		},
		{
			Name:                  "many join/leaves, use latest",
			InitMemberships:       [][2]string{{alice, "join"}, {charlie, "join"}, {frank, "join"}},
			AccumulateMemberships: [][2]string{{bob, "join"}, {charlie, "leave"}, {frank, "leave"}, {charlie, "join"}, {eve, "join"}},
			WantJoined:            []string{alice, bob, charlie, eve},
		},
		{
			Name:                  "many invites, use latest",
			InitMemberships:       [][2]string{{alice, "join"}, {doris, "join"}},
			AccumulateMemberships: [][2]string{{doris, "leave"}, {charlie, "invite"}, {doris, "invite"}},
			WantJoined:            []string{alice},
			WantInvited:           []string{charlie, doris},
		},
		{
			Name:                  "invite and rejection in accumulate",
			InitMemberships:       [][2]string{{alice, "join"}},
			AccumulateMemberships: [][2]string{{frank, "invite"}, {frank, "leave"}},
			WantJoined:            []string{alice},
		},
		{
			Name:                  "invite in initial, rejection in accumulate",
			InitMemberships:       [][2]string{{alice, "join"}, {frank, "invite"}},
			AccumulateMemberships: [][2]string{{frank, "leave"}},
			WantJoined:            []string{alice},
		},
	}

	serialise := func(memberships [][2]string) []json.RawMessage {
		var result []json.RawMessage
		for _, userWithMembership := range memberships {
			target := userWithMembership[0]
			sender := userWithMembership[0]
			membership := userWithMembership[1]
			if membership == "invite" {
				// Alice is always the inviter
				sender = alice
			}
			result = append(result, testutils.NewStateEvent(t, "m.room.member", target, sender, map[string]interface{}{
				"membership": membership,
			}))
		}
		return result
	}

	for i, tc := range testCases {
		roomID := fmt.Sprintf("!TestAllJoinedMembers_%d:localhost", i)
		_, err := store.Initialise(roomID, append([]json.RawMessage{
			testutils.NewStateEvent(t, "m.room.create", "", alice, map[string]interface{}{
				"creator": alice, // alice is always the creator
			}),
		}, serialise(tc.InitMemberships)...))
		assertNoError(t, err)

		_, err = store.Accumulate(userID, roomID, sync2.TimelineResponse{
			Events:    serialise(tc.AccumulateMemberships),
			PrevBatch: "foo",
		})
		assertNoError(t, err)
		testCases[i].RoomID = roomID // remember this for later
	}

	// should get all joined members correctly
	var joinedMembers map[string][]string
	// should set join/invite counts correctly
	var roomMetadatas map[string]internal.RoomMetadata
	err := sqlutil.WithTransaction(store.DB, func(txn *sqlx.Tx) error {
		tableName, err := store.PrepareSnapshot(txn)
		if err != nil {
			return err
		}
		joinedMembers, roomMetadatas, err = store.AllJoinedMembers(txn, tableName)
		return err
	})
	assertNoError(t, err)

	for _, tc := range testCases {
		roomID := tc.RoomID
		if roomID == "" {
			t.Fatalf("test case has no room id set: %+v", tc)
		}
		// make sure joined members match
		sort.Strings(joinedMembers[roomID])
		sort.Strings(tc.WantJoined)
		if !reflect.DeepEqual(joinedMembers[roomID], tc.WantJoined) {
			t.Errorf("%v: got joined members %v want %v", tc.Name, joinedMembers[roomID], tc.WantJoined)
		}
		// make sure join/invite counts match
		wantJoined := len(tc.WantJoined)
		wantInvited := len(tc.WantInvited)
		metadata, ok := roomMetadatas[roomID]
		if !ok {
			t.Fatalf("no room metadata for room %v", roomID)
		}
		if metadata.InviteCount != wantInvited {
			t.Errorf("%v: got invite count %d want %d", tc.Name, metadata.InviteCount, wantInvited)
		}
		if metadata.JoinCount != wantJoined {
			t.Errorf("%v: got join count %d want %d", tc.Name, metadata.JoinCount, wantJoined)
		}
	}
}

func TestCircularSlice(t *testing.T) {
	testCases := []struct {
		name    string
		max     int
		appends []int64
		want    []int64 // these get sorted in the test
	}{
		{
			name:    "wraparound",
			max:     5,
			appends: []int64{9, 8, 7, 6, 5, 4, 3, 2},
			want:    []int64{2, 3, 4, 5, 6},
		},
		{
			name:    "exact",
			max:     5,
			appends: []int64{9, 8, 7, 6, 5},
			want:    []int64{5, 6, 7, 8, 9},
		},
		{
			name:    "unfilled",
			max:     5,
			appends: []int64{9, 8, 7},
			want:    []int64{7, 8, 9},
		},
		{
			name:    "wraparound x2",
			max:     5,
			appends: []int64{9, 8, 7, 6, 5, 4, 3, 2, 1, 0, 10},
			want:    []int64{0, 1, 2, 3, 10},
		},
	}
	for _, tc := range testCases {
		cs := &circularSlice[int64]{
			max: tc.max,
		}
		for _, val := range tc.appends {
			cs.append(val)
		}
		sort.Slice(cs.vals, func(i, j int) bool {
			return cs.vals[i] < cs.vals[j]
		})
		if !reflect.DeepEqual(cs.vals, tc.want) {
			t.Errorf("%s: got %v want %v", tc.name, cs.vals, tc.want)
		}

	}

}

func TestStorage_FetchMemberships(t *testing.T) {
	assertNoError(t, cleanDB(t))
	store := NewStorage(postgresConnectionString)
	defer store.Teardown()

	events := []json.RawMessage{
		testutils.NewStateEvent(t, "m.room.create", "", "@alice:test", map[string]any{}),
		testutils.NewStateEvent(t, "m.room.member", "@alice:test", "@alice:test", map[string]any{"membership": "join"}),
		testutils.NewStateEvent(t, "m.room.member", "@brian:test", "@alice:test", map[string]any{"membership": "invite"}),
		testutils.NewStateEvent(t, "m.room.member", "@chris:test", "@chris:test", map[string]any{"membership": "leave"}),
		testutils.NewStateEvent(t, "m.room.member", "@david:test", "@alice:test", map[string]any{"membership": "ban"}),
		testutils.NewStateEvent(t, "m.room.member", "@erika:test", "@erika:test", map[string]any{"membership": "join"}),
		testutils.NewStateEvent(t, "m.room.member", "@frank:test", "@erika:test", map[string]any{"membership": "invite"}),
		testutils.NewStateEvent(t, "m.room.member", "@glory:test", "@glory:test", map[string]any{"membership": "leave"}),
		testutils.NewStateEvent(t, "m.room.member", "@helen:test", "@alice:test", map[string]any{"membership": "ban"}),
	}

	const roomID = "!unimportant"
	err := sqlutil.WithTransaction(store.DB, func(txn *sqlx.Tx) (err error) {
		_, err = store.Accumulator.Initialise(roomID, events)
		return err
	})
	assertNoError(t, err)

	joins, invites, leaves, err := store.FetchMemberships(roomID)
	assertNoError(t, err)

	// Do not assume an order from the DB.
	sort.Slice(joins, func(i, j int) bool {
		return joins[i] < joins[j]
	})
	sort.Slice(invites, func(i, j int) bool {
		return invites[i] < invites[j]
	})
	sort.Slice(leaves, func(i, j int) bool {
		return leaves[i] < leaves[j]
	})

	assertValue(t, "joins", joins, []string{"@alice:test", "@erika:test"})
	assertValue(t, "invites", invites, []string{"@brian:test", "@frank:test"})
	assertValue(t, "joins", leaves, []string{"@chris:test", "@david:test", "@glory:test", "@helen:test"})
}

type persistOpts struct {
	withInitialEvents bool
	numTimelineEvents int
	ofWhichNumState   int
}

func mustPersistEvents(t *testing.T, roomID string, store *Storage, opts persistOpts) {
	t.Helper()
	var events []json.RawMessage
	if opts.withInitialEvents {
		events = createInitialEvents(t, userID)
	}
	numAddedStateEvents := 0
	for i := 0; i < opts.numTimelineEvents; i++ {
		var ev json.RawMessage
		if numAddedStateEvents < opts.ofWhichNumState {
			numAddedStateEvents++
			ev = testutils.NewStateEvent(t, "some_kind_of_state", fmt.Sprintf("%d", rand.Int63()), userID, map[string]interface{}{
				"num": numAddedStateEvents,
			})
		} else {
			ev = testutils.NewEvent(t, "some_kind_of_message", userID, map[string]interface{}{
				"msg": "yep",
			})
		}
		events = append(events, ev)
	}
	mustAccumulate(t, store, roomID, events)
}

func mustAccumulate(t *testing.T, store *Storage, roomID string, events []json.RawMessage) {
	t.Helper()
	_, err := store.Accumulate(userID, roomID, sync2.TimelineResponse{
		Events: events,
	})
	if err != nil {
		t.Fatalf("Failed to accumulate: %s", err)
	}
}

func mustHaveNumSnapshots(t *testing.T, db *sqlx.DB, roomID string, numSnapshots int) {
	t.Helper()
	var val int
	err := db.QueryRow(`SELECT count(*) FROM syncv3_snapshots WHERE room_id=$1`, roomID).Scan(&val)
	if err != nil {
		t.Fatalf("mustHaveNumSnapshots: %s", err)
	}
	if val != numSnapshots {
		t.Fatalf("mustHaveNumSnapshots: got %d want %d snapshots", val, numSnapshots)
	}
}

func mustNotError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	t.Fatalf("err: %s", err)
}

func TestRemoveInaccessibleStateSnapshots(t *testing.T) {
	store := NewStorage(postgresConnectionString)
	store.MaxTimelineLimit = 50 // we nuke if we have >50+1 snapshots

	roomOnlyMessages := "!TestRemoveInaccessibleStateSnapshots_roomOnlyMessages:localhost"
	mustPersistEvents(t, roomOnlyMessages, store, persistOpts{
		withInitialEvents: true,
		numTimelineEvents: 100,
		ofWhichNumState:   0,
	})
	roomOnlyState := "!TestRemoveInaccessibleStateSnapshots_roomOnlyState:localhost"
	mustPersistEvents(t, roomOnlyState, store, persistOpts{
		withInitialEvents: true,
		numTimelineEvents: 100,
		ofWhichNumState:   100,
	})
	roomPartialStateAndMessages := "!TestRemoveInaccessibleStateSnapshots_roomPartialStateAndMessages:localhost"
	mustPersistEvents(t, roomPartialStateAndMessages, store, persistOpts{
		withInitialEvents: true,
		numTimelineEvents: 100,
		ofWhichNumState:   30,
	})
	roomOverwriteState := "TestRemoveInaccessibleStateSnapshots_roomOverwriteState:localhost"
	mustPersistEvents(t, roomOverwriteState, store, persistOpts{
		withInitialEvents: true,
	})
	mustAccumulate(t, store, roomOverwriteState, []json.RawMessage{testutils.NewStateEvent(t, "overwrite", "", userID, map[string]interface{}{"val": 1})})
	mustAccumulate(t, store, roomOverwriteState, []json.RawMessage{testutils.NewStateEvent(t, "overwrite", "", userID, map[string]interface{}{"val": 2})})
	mustHaveNumSnapshots(t, store.DB, roomOnlyMessages, 4)             // initial state only, one for each state event
	mustHaveNumSnapshots(t, store.DB, roomOnlyState, 104)              // initial state + 100 state events
	mustHaveNumSnapshots(t, store.DB, roomPartialStateAndMessages, 34) // initial state + 30 state events
	mustHaveNumSnapshots(t, store.DB, roomOverwriteState, 6)           // initial state + 2 overwrite state events
	mustNotError(t, store.RemoveInaccessibleStateSnapshots())
	mustHaveNumSnapshots(t, store.DB, roomOnlyMessages, 4)             // it should not be touched as 4 < 51
	mustHaveNumSnapshots(t, store.DB, roomOnlyState, 51)               // it should be capped at 51
	mustHaveNumSnapshots(t, store.DB, roomPartialStateAndMessages, 34) // it should not be touched as 34 < 51
	mustHaveNumSnapshots(t, store.DB, roomOverwriteState, 6)           // it should not be touched as 6 < 51
	// calling it again does nothing
	mustNotError(t, store.RemoveInaccessibleStateSnapshots())
	mustHaveNumSnapshots(t, store.DB, roomOnlyMessages, 4)
	mustHaveNumSnapshots(t, store.DB, roomOnlyState, 51)
	mustHaveNumSnapshots(t, store.DB, roomPartialStateAndMessages, 34)
	mustHaveNumSnapshots(t, store.DB, roomOverwriteState, 6) // it should not be touched as 6 < 51
	// adding one extra state snapshot to each room and repeating RemoveInaccessibleStateSnapshots
	mustPersistEvents(t, roomOnlyMessages, store, persistOpts{numTimelineEvents: 1, ofWhichNumState: 1})
	mustPersistEvents(t, roomOnlyState, store, persistOpts{numTimelineEvents: 1, ofWhichNumState: 1})
	mustPersistEvents(t, roomPartialStateAndMessages, store, persistOpts{numTimelineEvents: 1, ofWhichNumState: 1})
	mustNotError(t, store.RemoveInaccessibleStateSnapshots())
	mustHaveNumSnapshots(t, store.DB, roomOnlyMessages, 5)
	mustHaveNumSnapshots(t, store.DB, roomOnlyState, 51) // still capped
	mustHaveNumSnapshots(t, store.DB, roomPartialStateAndMessages, 35)
	// adding 51 timeline events and repeating RemoveInaccessibleStateSnapshots does nothing
	mustPersistEvents(t, roomOnlyMessages, store, persistOpts{numTimelineEvents: 51})
	mustPersistEvents(t, roomOnlyState, store, persistOpts{numTimelineEvents: 51})
	mustPersistEvents(t, roomPartialStateAndMessages, store, persistOpts{numTimelineEvents: 51})
	mustNotError(t, store.RemoveInaccessibleStateSnapshots())
	mustHaveNumSnapshots(t, store.DB, roomOnlyMessages, 5)
	mustHaveNumSnapshots(t, store.DB, roomOnlyState, 51)
	mustHaveNumSnapshots(t, store.DB, roomPartialStateAndMessages, 35)

	// overwrite 52 times and check the current state is 52 (shows we are keeping the right snapshots)
	for i := 0; i < 52; i++ {
		mustAccumulate(t, store, roomOverwriteState, []json.RawMessage{testutils.NewStateEvent(t, "overwrite", "", userID, map[string]interface{}{"val": 1 + i})})
	}
	mustHaveNumSnapshots(t, store.DB, roomOverwriteState, 58)
	mustNotError(t, store.RemoveInaccessibleStateSnapshots())
	mustHaveNumSnapshots(t, store.DB, roomOverwriteState, 51)
	roomsTable := NewRoomsTable(store.DB)
	mustNotError(t, sqlutil.WithTransaction(store.DB, func(txn *sqlx.Tx) error {
		snapID, err := roomsTable.CurrentAfterSnapshotID(txn, roomOverwriteState)
		if err != nil {
			return err
		}
		state, err := store.StateSnapshot(snapID)
		if err != nil {
			return err
		}
		// find the 'overwrite' event and make sure the val is 52
		for _, ev := range state {
			evv := gjson.ParseBytes(ev)
			if evv.Get("type").Str != "overwrite" {
				continue
			}
			if evv.Get("content.val").Int() != 52 {
				return fmt.Errorf("val for overwrite state event was not 52: %v", evv.Raw)
			}
		}
		return nil
	}))
}

func createInitialEvents(t *testing.T, creator string) []json.RawMessage {
	t.Helper()
	baseTimestamp := time.Now()
	var pl gomatrixserverlib.PowerLevelContent
	pl.Defaults()
	pl.Users = map[string]int64{
		creator: 100,
	}
	// all with the same timestamp as they get made atomically
	return []json.RawMessage{
		testutils.NewStateEvent(t, "m.room.create", "", creator, map[string]interface{}{"creator": creator}, testutils.WithTimestamp(baseTimestamp)),
		testutils.NewJoinEvent(t, creator, testutils.WithTimestamp(baseTimestamp)),
		testutils.NewStateEvent(t, "m.room.power_levels", "", creator, pl, testutils.WithTimestamp(baseTimestamp)),
		testutils.NewStateEvent(t, "m.room.join_rules", "", creator, map[string]interface{}{"join_rule": "public"}, testutils.WithTimestamp(baseTimestamp)),
	}
}

func cleanDB(t *testing.T) error {
	// make a fresh DB which is unpolluted from other tests
	db, close := connectToDB(t)
	_, err := db.Exec(`
	DROP TABLE IF EXISTS syncv3_rooms;
	DROP TABLE IF EXISTS syncv3_invites;
	DROP TABLE IF EXISTS syncv3_snapshots;
	DROP TABLE IF EXISTS syncv3_spaces;`)
	close()
	return err
}

func assertRoomMetadata(t *testing.T, got, want internal.RoomMetadata) {
	t.Helper()
	assertValue(t, "CanonicalAlias", got.CanonicalAlias, want.CanonicalAlias)
	assertValue(t, "ChildSpaceRooms", got.ChildSpaceRooms, want.ChildSpaceRooms)
	assertValue(t, "Encrypted", got.Encrypted, want.Encrypted)
	assertValue(t, "Heroes", sortHeroes(got.Heroes), sortHeroes(want.Heroes))
	assertValue(t, "InviteCount", got.InviteCount, want.InviteCount)
	assertValue(t, "JoinCount", got.JoinCount, want.JoinCount)
	assertValue(t, "LastMessageTimestamp", got.LastMessageTimestamp, want.LastMessageTimestamp)
	assertValue(t, "NameEvent", got.NameEvent, want.NameEvent)
	assertValue(t, "PredecessorRoomID", got.PredecessorRoomID, want.PredecessorRoomID)
	assertValue(t, "RoomID", got.RoomID, want.RoomID)
	assertValue(t, "RoomType", got.RoomType, want.RoomType)
	assertValue(t, "TypingEvent", got.TypingEvent, want.TypingEvent)
	assertValue(t, "UpgradedRoomID", got.UpgradedRoomID, want.UpgradedRoomID)
}

func assertValue(t *testing.T, msg string, got, want interface{}) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v want %v", msg, got, want)
	}
}

func sortHeroes(heroes []internal.Hero) []internal.Hero {
	sort.Slice(heroes, func(i, j int) bool {
		return heroes[i].ID < heroes[j].ID
	})
	return heroes
}

func verifyRange(t *testing.T, result map[string][2]int64, roomID string, wantRange [2]int64) {
	t.Helper()
	gotRange := result[roomID]
	if gotRange == [2]int64{} {
		t.Fatalf("no range was returned for room %s", roomID)
	}
	if !reflect.DeepEqual(gotRange, wantRange) {
		t.Errorf("%s range got %v want %v", roomID, gotRange, wantRange)
	}
}
