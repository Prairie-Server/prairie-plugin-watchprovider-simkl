package provider

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	opMarkWatched   = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED
	opMarkUnwatched = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED
)

// historyFake serves the watched lists from movies and shows, which hold
// whatever POST /sync/history added, and records history writes.
type historyFake struct {
	t        *testing.T
	mu       sync.Mutex
	movies   []map[string]any
	shows    []map[string]any
	notFound string
	writes   []map[string]any
	paths    []string
}

func (f *historyFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.paths = append(f.paths, r.Method+" "+r.URL.Path)
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/sync/all-items/movies/completed":
		writeJSON(f.t, w, map[string]any{"movies": f.movies})
	case r.Method == http.MethodGet && r.URL.Path == "/sync/all-items/shows/completed":
		writeJSON(f.t, w, map[string]any{"shows": f.shows})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sync/all-items/"):
		writeJSON(f.t, w, `{}`)
	case r.Method == http.MethodPost && (r.URL.Path == "/sync/history" || r.URL.Path == "/sync/history/remove"):
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode body: %v", err)
		}
		f.writes = append(f.writes, body)
		if r.URL.Path == "/sync/history" {
			movies, _ := body["movies"].([]any)
			for _, movie := range movies {
				entry := movie.(map[string]any)
				f.movies = append(f.movies, map[string]any{
					"status": "completed", "last_watched_at": entry["watched_at"],
					"movie": map[string]any{"ids": entry["ids"]},
				})
			}
		}
		notFound := f.notFound
		if notFound == "" {
			notFound = `{"movies":[],"shows":[],"episodes":[]}`
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(f.t, w, `{"not_found":`+notFound+`}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
	}
}

func (f *historyFake) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func moviePlay(id string, at time.Time, ids map[string]string) *pluginv1.WatchSyncEvent {
	event := movieEvent(id, opMarkWatched, ids)
	event.WatchHistoryId = id
	event.OccurredAt = timestamp(at)
	if imdb := ids["imdb"]; imdb != "" {
		event.ProviderItemKey = "imdb:" + imdb
	}
	return event
}

func episodePlay(id string, at time.Time) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:         id,
		Operation:       opMarkWatched,
		OccurredAt:      timestamp(at),
		WatchHistoryId:  id,
		ProviderItemKey: "tvdb:67890",
		Media: &pluginv1.WatchSyncMedia{
			MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
			ExternalIds:       map[string]string{"tvdb": "67890"},
			SeriesTitle:       "Missing Show",
			SeriesYear:        2024,
			SeriesExternalIds: map[string]string{"tvdb": "12345"},
			SeasonNumber:      2,
			EpisodeNumber:     3,
		},
	}
}

func TestMarkWatchedSendsSimklPayload(t *testing.T) {
	fake := &historyFake{t: t}
	server, _ := newTestServer(t, fake)
	event := moviePlay("history-1", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"})
	event.Media.Title, event.Media.Year = "Inception", 2010

	response := applyEvents(t, server, event)
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if status := resultsByID(response)["history-1"].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("status = %v", status)
	}
	if len(fake.writes) != 1 {
		t.Fatalf("writes = %v", fake.writes)
	}
	movies, _ := fake.writes[0]["movies"].([]any)
	if len(movies) != 1 {
		t.Fatalf("body = %#v, want one movie", fake.writes[0])
	}
	movie := movies[0].(map[string]any)
	if movie["watched_at"] != "2026-05-04T12:00:00Z" || movie["title"] != "Inception" || movie["year"] != float64(2010) ||
		movie["ids"].(map[string]any)["imdb"] != "tt1375666" {
		t.Fatalf("movie payload = %#v", movie)
	}
	// A movie-only batch checks the movie list only.
	for _, path := range fake.paths {
		if strings.Contains(path, "shows") || strings.Contains(path, "anime") {
			t.Fatalf("requests = %v, want no episode lists", fake.paths)
		}
	}
}

func TestMarkWatchedMapsSimklNotFoundItems(t *testing.T) {
	fake := &historyFake{t: t, notFound: `{
		"movies": [{"title": "Missing Movie", "year": 2026, "watched_at": "2026-05-04T12:00:00Z", "ids": {"imdb": "tt0000001"}}],
		"shows": [{"title": "Missing Show", "year": 2024, "ids": {"tvdb": "12345"},
			"seasons": [{"number": 2, "episodes": [{"number": 3, "watched_at": "2026-05-04T13:00:00Z", "ids": {"tvdb": "67890"}}]}]}],
		"episodes": []
	}`}
	server, _ := newTestServer(t, fake)
	known := moviePlay("movie-ok", time.Date(2026, 5, 4, 11, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"})
	known.Media.Title, known.Media.Year = "Known Movie", 2010
	missing := moviePlay("movie-missing", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt0000001"})
	missing.Media.Title, missing.Media.Year = "Missing Movie", 2026

	response := applyEvents(t, server, known, missing, episodePlay("episode-missing", time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC)))
	results := resultsByID(response)
	if results["movie-ok"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("movie-ok = %v", results["movie-ok"])
	}
	for _, id := range []string{"movie-missing", "episode-missing"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED ||
			results[id].GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
			t.Fatalf("%s = %v, want rejected as not found", id, results[id])
		}
	}
}

func TestMarkWatchedSkipsPlaysSimklAlreadyHas(t *testing.T) {
	fake := &historyFake{
		t: t,
		movies: []map[string]any{{
			"status": "completed", "last_watched_at": "2026-05-04T12:00:00.400Z",
			"movie": map[string]any{"ids": map[string]any{"imdb": "tt1375666"}},
		}},
		shows: []map[string]any{{
			"status": "completed",
			"show":   map[string]any{"ids": map[string]any{"tvdb": 12345}},
			"seasons": []any{map[string]any{"number": 2, "episodes": []any{map[string]any{
				"number": 3, "watched_at": "2026-05-04T13:00:00Z", "ids": map[string]any{"tvdb": 67890},
			}}}},
		}},
	}
	server, _ := newTestServer(t, fake)
	response := applyEvents(t, server,
		moviePlay("same-movie", time.Date(2026, 5, 4, 12, 0, 0, 900_000_000, time.UTC), map[string]string{"imdb": "tt1375666"}),
		episodePlay("same-episode", time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC)),
		moviePlay("rewatch", time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"}),
	)
	results := resultsByID(response)
	for _, id := range []string{"same-movie", "same-episode"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
			t.Fatalf("%s = %v, want no change", id, results[id])
		}
	}
	if results["rewatch"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("rewatch = %v", results["rewatch"])
	}
	movies, _ := fake.writes[0]["movies"].([]any)
	if len(fake.writes) != 1 || len(movies) != 1 || fake.writes[0]["shows"] != nil ||
		movies[0].(map[string]any)["watched_at"] != "2026-05-05T12:00:00Z" {
		t.Fatalf("writes = %#v, want only the rewatch", fake.writes)
	}
}

func TestRedeliveredMarkWatchedDoesNotWriteTwice(t *testing.T) {
	fake := &historyFake{t: t}
	server, _ := newTestServer(t, fake)
	event := moviePlay("history-1", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"})

	first := resultsByID(applyEvents(t, server, event))["history-1"]
	second := resultsByID(applyEvents(t, server, event))["history-1"]
	if first.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED ||
		second.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("first = %v second = %v", first, second)
	}
	if fake.writeCount() != 1 {
		t.Fatalf("writes = %d, want 1", fake.writeCount())
	}
}

func TestMarkWatchedMatchesOneRemotePlayPerLocalPlay(t *testing.T) {
	at := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	fake := &historyFake{t: t, movies: []map[string]any{{
		"status": "completed", "last_watched_at": "2026-05-04T12:00:00Z",
		"movie": map[string]any{"ids": map[string]any{"imdb": "tt1"}},
	}}}
	server, _ := newTestServer(t, fake)
	results := resultsByID(applyEvents(t, server,
		moviePlay("a", at, map[string]string{"imdb": "tt1"}),
		moviePlay("b", at, map[string]string{"imdb": "tt1"}),
	))
	if results["a"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE ||
		results["b"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("results = %v", results)
	}
}

func TestMarkWatchedDerivesTheKeyWhenTheEventHasNone(t *testing.T) {
	fake := &historyFake{t: t, shows: []map[string]any{{
		"status": "watching",
		"show":   map[string]any{"ids": map[string]any{"tvdb": 12345}},
		"seasons": []any{map[string]any{"number": 2, "episodes": []any{map[string]any{
			"number": 3, "watched_at": "2026-05-04T13:00:00Z",
		}}}},
	}}}
	server, _ := newTestServer(t, fake)
	event := episodePlay("e", time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC))
	event.ProviderItemKey = ""
	event.Media.ExternalIds = nil
	if status := resultsByID(applyEvents(t, server, event))["e"].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("status = %v, want no change for show:tvdb:12345:s2:e3", status)
	}
}

func TestMarkUnwatchedRemovesWithoutWatchedAt(t *testing.T) {
	fake := &historyFake{t: t, notFound: `{"movies":[{"ids":{"imdb":"tt0000001"}}],"shows":[],"episodes":[]}`}
	server, _ := newTestServer(t, fake)
	gone := moviePlay("unwatched:a", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"})
	gone.Operation = opMarkUnwatched
	missing := moviePlay("unwatched:b", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt0000001"})
	missing.Operation = opMarkUnwatched

	results := resultsByID(applyEvents(t, server, gone, missing))
	if results["unwatched:a"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED ||
		results["unwatched:b"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("results = %v", results)
	}
	if len(fake.paths) != 1 || fake.paths[0] != "POST /sync/history/remove" {
		t.Fatalf("requests = %v, want one removal and no list reads", fake.paths)
	}
	for _, movie := range fake.writes[0]["movies"].([]any) {
		if _, ok := movie.(map[string]any)["watched_at"]; ok {
			t.Fatalf("removal payload = %#v, want no watched_at", fake.writes[0])
		}
	}
}

func TestMarkWatchedRejectsSeriesWithoutCallingSimkl(t *testing.T) {
	fake := &historyFake{t: t}
	server, _ := newTestServer(t, fake)
	result := resultsByID(applyEvents(t, server, seriesEvent("s", opMarkWatched, map[string]string{"tvdb": "1"})))["s"]
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || len(fake.paths) != 0 {
		t.Fatalf("result = %v requests = %v", result, fake.paths)
	}
}

func TestRepeatedEventIDIsAppliedOnce(t *testing.T) {
	fake := &historyFake{t: t}
	server, _ := newTestServer(t, fake)
	event := moviePlay("same", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1"})
	response := applyEvents(t, server, event, event)
	movies, _ := fake.writes[0]["movies"].([]any)
	if len(response.GetResults()) != 1 || len(movies) != 1 {
		t.Fatalf("results = %v writes = %#v", response.GetResults(), fake.writes)
	}
}

func TestHistoryWriteFailureFailsTheBatch(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(t, w, `{}`)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(t, w, `{"error":"upstream `+testToken+`"}`)
	}))
	response := applyEvents(t, server, moviePlay("a", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1"}))
	fault := response.GetFault()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY || len(response.GetResults()) != 0 {
		t.Fatalf("response = %v, want a temporary batch fault", response)
	}
	assertSafe(t, fault)
}
