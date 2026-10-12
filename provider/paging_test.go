package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// completedShows is an all-items reply with one completed show of n watched
// episodes, numbered from first.
func completedShows(first, n int) string {
	episodes := make([]string, 0, n)
	for number := first; number < first+n; number++ {
		episodes = append(episodes, fmt.Sprintf(`{"number":%d,"title":"Episode %d","watched_at":"2026-01-01T00:00:00Z","ids":{"tvdb":%d,"tmdb":%d}}`, number, number, 100000+number, 500000+number))
	}
	return `{"shows":[{"status":"completed","last_watched_at":"2026-01-02T00:00:00Z","show":{"title":"Long Show","year":1999,"ids":{"tvdb":"81189","tmdb":"1396","imdb":"tt0903747"}},"seasons":[{"number":1,"episodes":[` + strings.Join(episodes, ",") + `]}]}]}`
}

// pagingServer serves the watched lists with a large completed-shows list,
// which body returns on each read, and counts that list's reads.
func pagingServer(t *testing.T, body func(read int) string) (*Server, *atomic.Int32) {
	t.Helper()
	var reads atomic.Int32
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sync/activities":
			writeJSON(t, w, watchedActivitiesFixture)
		case "/sync/all-items/shows/completed":
			writeJSON(t, w, body(int(reads.Add(1))))
		default:
			writeJSON(t, w, `{}`)
		}
	}))
	return server, &reads
}

func requireEpisodes(t *testing.T, items []*pluginv1.WatchSyncRemoteState, first, n int) {
	t.Helper()
	keys := map[string]bool{}
	for _, item := range items {
		if keys[item.GetProviderItemKey()] {
			t.Fatalf("row %s returned twice", item.GetProviderItemKey())
		}
		keys[item.GetProviderItemKey()] = true
	}
	for number := first; number < first+n; number++ {
		if key := fmt.Sprintf("tvdb:%d", 100000+number); !keys[key] {
			t.Fatalf("row %s missing from %d rows", key, len(items))
		}
	}
}

func TestLargeListIsReadOnceAndPagedFromTheToken(t *testing.T) {
	server, reads := pagingServer(t, func(int) string { return completedShows(1, 350) })
	result := listAll(t, server, kindWatched, "", 100)
	if len(result.items) != 350 {
		t.Fatalf("rows = %d, want 350", len(result.items))
	}
	requireEpisodes(t, result.items, 1, 350)
	if reads.Load() != 1 {
		t.Fatalf("completed shows read %d times, want once", reads.Load())
	}
	if result.pages < 4 {
		t.Fatalf("pages = %d, want the list split into pages of 100", result.pages)
	}
}

func TestOversizedListIsReReadPerWindowAndResumesAfterTheLastRow(t *testing.T) {
	server, reads := pagingServer(t, func(int) string { return completedShows(1, 350) })
	// About 50 rows fit the budget, so the list takes several windows.
	server.carryBudget = 50 * 130
	result := listAll(t, server, kindWatched, "", 25)
	if len(result.items) != 350 {
		t.Fatalf("rows = %d, want 350", len(result.items))
	}
	requireEpisodes(t, result.items, 1, 350)
	if reads.Load() < 3 || reads.Load() > 10 {
		t.Fatalf("completed shows read %d times, want one read per window", reads.Load())
	}
}

func TestWarningsAreCountedOncePerReadAcrossPagesAndReReads(t *testing.T) {
	const unkeyed = `{"status":"completed","show":{"title":"No IDs","ids":{}},"seasons":[{"number":1,"episodes":[` +
		`{"number":1,"watched_at":"2026-01-01T00:00:00Z"},{"number":2,"watched_at":"2026-01-01T00:00:00Z"}]}]}`
	server, reads := pagingServer(t, func(int) string {
		return strings.Replace(completedShows(1, 350), `{"shows":[`, `{"shows":[`+unkeyed+`,`, 1)
	})
	server.carryBudget = 50 * 130
	result := listAll(t, server, kindWatched, "", 25)
	requireEpisodes(t, result.items, 1, 350)
	if reads.Load() < 3 {
		t.Fatalf("completed shows read %d times, want several windows", reads.Load())
	}
	if want := []string{warnWatchedEpisodeNoID + " (2 items)"}; !reflect.DeepEqual(result.warnings, want) {
		t.Fatalf("warnings = %q, want %q", result.warnings, want)
	}
}

func TestDeltaReReadToleratesAListThatChanged(t *testing.T) {
	// Each re-read adds an episode. A delta read may miss it: the change moves
	// the list's activity past the cursor this traversal records, so the next
	// traversal reads it.
	server, _ := pagingServer(t, func(read int) string { return completedShows(1-read, 300+read) })
	server.carryBudget = 50 * 130
	result := listAll(t, server, kindWatched, "", 25)
	requireEpisodes(t, result.items, 1, 300)
}

func TestSnapshotReReadOfAChangedListFailsAsTemporary(t *testing.T) {
	movies := func(n int) string {
		entries := make([]string, 0, n)
		for index := range n {
			entries = append(entries, fmt.Sprintf(`{"movie":{"title":"Movie %d","year":2000,"ids":{"imdb":"tt%07d"}}}`, index, index))
		}
		return `{"movies":[` + strings.Join(entries, ",") + `]}`
	}
	var mu sync.Mutex
	reads := 0
	changed := true
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sync/all-items/movies/plantowatch" {
			writeJSON(t, w, `{}`)
			return
		}
		mu.Lock()
		reads++
		n := 300
		if changed && reads > 1 {
			n = 299
		}
		mu.Unlock()
		writeJSON(t, w, movies(n))
	}))
	server.carryBudget = 50 * 40
	_, fault := listAllWithFault(t, server, kindWatchlist, "", 25)
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || fault.GetSafeMessage() != listChangedMessage {
		t.Fatalf("fault = %v, want a temporary fault for the changed list", fault)
	}

	mu.Lock()
	reads, changed = 0, false
	mu.Unlock()
	result := listAll(t, server, kindWatchlist, "", 25)
	if len(result.items) != 300 || !result.complete || reads < 2 {
		t.Fatalf("rows = %d complete = %v reads = %d, want the whole unchanged list over several reads", len(result.items), result.complete, reads)
	}
}

func TestPageTokenMustBelongToTheTraversal(t *testing.T) {
	server, _ := pagingServer(t, func(int) string { return completedShows(1, 150) })
	first, err := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: authContext(), PageSize: 100, StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched},
	})
	if err != nil || first.GetNextPageToken() == "" {
		t.Fatalf("first page = %v %v, want a page token", first, err)
	}
	for name, request := range map[string]*pluginv1.WatchSyncListRemoteStateRequest{
		"other kind":   {PageToken: first.GetNextPageToken(), StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindProgress}},
		"other cursor": {PageToken: first.GetNextPageToken(), Cursor: `{"x":"y"}`, StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched}},
		"garbage":      {PageToken: "not-a-token", StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched}},
	} {
		request.Context = authContext()
		response, err := server.ListRemoteState(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
			t.Fatalf("%s: fault = %v, want invalid request", name, response.GetFault())
		}
	}
}

func TestPageTokenStaysSmallForALargeLibrary(t *testing.T) {
	server, reads := pagingServer(t, func(int) string { return completedShows(1, 6000) })
	response, err := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
		Context: authContext(), PageSize: 100, StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched},
	})
	if err != nil || response.GetFault() != nil {
		t.Fatalf("ListRemoteState: %v %v", err, response.GetFault())
	}
	// Page 1 reads the movie list, so the token holds no rows yet. Follow it
	// to the page that read the large list.
	token := response.GetNextPageToken()
	largest := 0
	for page := 0; page < 5 && token != ""; page++ {
		response, err = server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context: authContext(), PageSize: 100, PageToken: token, StateKinds: []pluginv1.WatchSyncRemoteStateKind{kindWatched},
		})
		if err != nil || response.GetFault() != nil {
			t.Fatalf("ListRemoteState: %v %v", err, response.GetFault())
		}
		token = response.GetNextPageToken()
		largest = max(largest, len(token))
	}
	if reads.Load() != 1 {
		t.Fatalf("large list read %d times so far, want once", reads.Load())
	}
	if largest == 0 || largest > 256<<10 {
		t.Fatalf("largest page token = %d bytes, want under 256 KiB", largest)
	}
	t.Logf("largest page token: %d bytes", largest)
}
