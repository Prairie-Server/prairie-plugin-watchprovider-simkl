package provider

import (
	"net/http"
	"reflect"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	kindDropped     = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_DROPPED
	opMarkDropped   = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED
	opUnmarkDropped = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_UNMARK_DROPPED

	droppedActivitiesFixture = `{"tv_shows":{"all":"2026-05-04T12:00:00Z","dropped":"2026-05-01T00:00:00Z"},"anime":{"all":"2026-05-04T12:10:00Z"}}`
)

func droppedLists(t *testing.T, activities string) *fakeSimkl {
	return &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities":              activities,
		"/sync/all-items/shows/dropped": `{"shows":[{"status":"dropped","show":{"title":"Rick and Morty","year":2013,"ids":{"simkl":1,"imdb":"tt2861424","tmdb":60625,"tvdb":275274}}}]}`,
		"/sync/all-items/anime/dropped": `{"anime":[
			{"status":"dropped","anime_type":"tv","show":{"title":"Cowboy Bebop","year":1998,"ids":{"simkl":37089,"mal":"1","tmdb":"30991"}}},
			{"status":"dropped","show":{"title":"No IDs","ids":{}}}
		]}`,
	}}
}

func TestListDroppedReadsShowsAndAnimeAsACompleteSnapshot(t *testing.T) {
	fake := droppedLists(t, droppedActivitiesFixture)
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindDropped, "", 100)
	if !result.complete || len(result.items) != 2 {
		t.Fatalf("result = %+v, want two series in a complete read", result)
	}
	rows := rowsByKey(t, result.items)
	if show := rows["tvdb:275274"]; show != (row{
		ProviderItemKey: "tvdb:275274", Kind: "series", Title: "Rick and Morty", Year: 2013,
		IMDbID: "tt2861424", TMDBID: "60625", TVDBID: "275274",
	}) {
		t.Fatalf("show row = %#v", show)
	}
	if anime := rows["tmdb:30991"]; anime != (row{ProviderItemKey: "tmdb:30991", Kind: "series", Title: "Cowboy Bebop", Year: 1998, TMDBID: "30991"}) {
		t.Fatalf("anime row = %#v", anime)
	}
	for _, item := range result.items {
		// Simkl records no time a show was dropped.
		if dropped := item.GetDropped(); dropped == nil || dropped.GetListedAt() != nil || dropped.GetRemoved() {
			t.Fatalf("dropped state = %v, want a drop without a time", dropped)
		}
	}
	want := []string{"/sync/activities", "/sync/all-items/shows/dropped?extended=full", "/sync/all-items/anime/dropped?extended=full"}
	if requests := fake.requests(); !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
	wantCursor := map[string]string{cursorDroppedShows: "2026-05-04T12:00:00Z", cursorDroppedAnime: "2026-05-04T12:10:00Z"}
	if got := decodedCursor(t, result.cursor); !reflect.DeepEqual(got, wantCursor) {
		t.Fatalf("cursor = %v, want %v", got, wantCursor)
	}
	if len(result.warnings) != 0 {
		t.Fatalf("warnings = %v, want none", result.warnings)
	}
}

func TestListDroppedSkipsUnchangedActivity(t *testing.T) {
	fake := droppedLists(t, droppedActivitiesFixture)
	server, _ := newTestServer(t, fake)
	cursor := map[string]string{cursorDroppedShows: "2026-05-04T12:00:00Z", cursorDroppedAnime: "2026-05-04T12:10:00Z"}
	result := listAll(t, server, kindDropped, encodedCursor(t, cursor), 100)
	if requests := fake.requests(); !reflect.DeepEqual(requests, []string{"/sync/activities"}) {
		t.Fatalf("requests = %v, want only the activities read", requests)
	}
	if result.complete || len(result.items) != 0 || !reflect.DeepEqual(decodedCursor(t, result.cursor), cursor) {
		t.Fatalf("result = %+v, want an incomplete empty read and the same cursor", result)
	}
}

func TestListDroppedReadsBothListsInFullWhenEitherStampMoved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cursor map[string]string
	}{
		// A show leaving the dropped list moves the "all" stamp, not the
		// "dropped" one, so the "all" stamp decides.
		{name: "shows moved", cursor: map[string]string{cursorDroppedShows: "2026-05-01T00:00:00Z", cursorDroppedAnime: "2026-05-04T12:10:00Z"}},
		{name: "anime moved", cursor: map[string]string{cursorDroppedShows: "2026-05-04T12:00:00Z", cursorDroppedAnime: "2026-05-01T00:00:00Z"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := droppedLists(t, droppedActivitiesFixture)
			server, _ := newTestServer(t, fake)
			result := listAll(t, server, kindDropped, encodedCursor(t, tc.cursor), 100)
			if fake.count("/sync/all-items/shows/dropped") != 1 || fake.count("/sync/all-items/anime/dropped") != 1 {
				t.Fatalf("requests = %v, want both lists read", fake.requests())
			}
			for _, uri := range fake.requests() {
				if containsDateFrom(uri) {
					t.Fatalf("dropped read sent date_from: %s", uri)
				}
			}
			if !result.complete || len(result.items) != 2 {
				t.Fatalf("result = %+v, want a complete read", result)
			}
		})
	}
}

func TestListDroppedRecordsOnlyTheStampsSimklReports(t *testing.T) {
	// Like the built-in provider, a list without a stamp is read again on the
	// next sync.
	fake := droppedLists(t, `{"tv_shows":{"all":"2026-05-04T12:00:00Z"},"anime":{"all":null}}`)
	server, _ := newTestServer(t, fake)
	cursor := encodedCursor(t, map[string]string{cursorDroppedShows: "2026-05-01T00:00:00Z", cursorDroppedAnime: "2026-05-01T00:00:00Z"})
	result := listAll(t, server, kindDropped, cursor, 100)
	if want := map[string]string{cursorDroppedShows: "2026-05-04T12:00:00Z"}; !reflect.DeepEqual(decodedCursor(t, result.cursor), want) {
		t.Fatalf("cursor = %v, want %v", result.cursor, want)
	}
	if !result.complete {
		t.Fatal("a read of both lists must be a complete snapshot")
	}
}

func TestDroppedChangesMoveSeriesBetweenLists(t *testing.T) {
	for _, tc := range []struct {
		operation pluginv1.WatchSyncOperation
		to        string
		noIDs     pluginv1.WatchSyncApplyStatus
	}{
		{opMarkDropped, "dropped", pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED},
		// Simkl cannot hold a drop of a title it could never match, and an
		// undrop must converge, so it changes nothing.
		{opUnmarkDropped, "watching", pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE},
	} {
		t.Run(tc.to, func(t *testing.T) {
			server, requests := recordingServer(t, http.StatusCreated, `{}`)
			agreed := seriesEvent("agreed", tc.operation, nil)
			// An agreed row for a removed media item carries only its key.
			agreed.ProviderItemKey = "tvdb:79126"
			events := []*pluginv1.WatchSyncEvent{
				seriesEvent("series", tc.operation, map[string]string{"tmdb": "1"}),
				agreed,
				movieEvent("movie", tc.operation, map[string]string{"tmdb": "2"}),
				seriesEvent("no-ids", tc.operation, nil),
			}
			// Sending the same change again is harmless and applies again.
			for range 2 {
				results := resultsByID(applyEvents(t, server, events...))
				for id, want := range map[string]pluginv1.WatchSyncApplyStatus{
					"series": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED,
					"agreed": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED,
					"movie":  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED,
					"no-ids": tc.noIDs,
				} {
					if results[id].GetStatus() != want {
						t.Fatalf("%s = %v, want %v", id, results[id], want)
					}
				}
			}
			if len(*requests) != 2 {
				t.Fatalf("requests = %+v, want one per call", *requests)
			}
			request := (*requests)[0]
			if request.method != http.MethodPost || request.path != "/sync/add-to-list" {
				t.Fatalf("request = %s %s", request.method, request.path)
			}
			want := map[string]any{"shows": []any{
				map[string]any{"to": tc.to, "ids": map[string]any{"tmdb": float64(1)}},
				map[string]any{"to": tc.to, "ids": map[string]any{"tvdb": float64(79126)}},
			}}
			if !reflect.DeepEqual(request.body, want) {
				t.Fatalf("body = %#v, want %#v", request.body, want)
			}
		})
	}
}

func TestDroppedChangeWithNothingToSendMakesNoRequest(t *testing.T) {
	server, requests := recordingServer(t, http.StatusCreated, `{}`)
	response := applyEvents(t, server,
		movieEvent("movie", opMarkDropped, map[string]string{"tmdb": "2"}),
		seriesEvent("no-ids", opUnmarkDropped, nil),
	)
	if len(*requests) != 0 || response.GetFault() != nil || len(response.GetResults()) != 2 {
		t.Fatalf("requests = %+v response = %v, want no request", *requests, response)
	}
}

func TestDroppedWriteFailureFailsTheBatch(t *testing.T) {
	server, _ := recordingServer(t, http.StatusBadGateway, `{"error":"upstream `+testToken+`"}`)
	response := applyEvents(t, server, seriesEvent("a", opMarkDropped, map[string]string{"tvdb": "1"}))
	fault := response.GetFault()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || len(response.GetResults()) != 0 {
		t.Fatalf("response = %v, want a temporary batch fault", response)
	}
	assertSafe(t, fault)
}
