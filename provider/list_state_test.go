package provider

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	kindWatched   = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED
	kindProgress  = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS
	kindWatchlist = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST
	kindRating    = pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING
)

const watchedActivitiesFixture = `{
	"movies":{"completed":"2026-05-04T12:00:00Z"},
	"tv_shows":{"watching":"2026-05-04T12:05:00Z","completed":"2026-05-04T12:10:00Z"},
	"anime":{"watching":"2026-05-04T12:15:00Z","completed":"2026-05-04T12:20:00Z"}
}`

// fakeSimkl answers GETs by path from bodies and records every request URI.
type fakeSimkl struct {
	t      *testing.T
	bodies map[string]string
	mu     sync.Mutex
	seen   []string
}

func (f *fakeSimkl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.seen = append(f.seen, r.URL.RequestURI())
	f.mu.Unlock()
	body, ok := f.bodies[r.URL.Path]
	if !ok {
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
		return
	}
	writeJSON(f.t, w, body)
}

func (f *fakeSimkl) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func (f *fakeSimkl) count(path string) int {
	n := 0
	for _, uri := range f.requests() {
		if uri == path || len(uri) > len(path) && uri[:len(path)] == path && uri[len(path)] == '?' {
			n++
		}
	}
	return n
}

func decodedCursor(t *testing.T, cursor string) map[string]string {
	t.Helper()
	decoded := map[string]string{}
	if cursor == "" {
		return decoded
	}
	if err := json.Unmarshal([]byte(cursor), &decoded); err != nil {
		t.Fatalf("cursor %q is not JSON: %v", cursor, err)
	}
	return decoded
}

func encodedCursor(t *testing.T, cursor map[string]string) string {
	t.Helper()
	encoded, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestListWatchedMapsMoviesShowsAndAnime(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities":                 `{"movies":{"completed":"2026-05-04T12:00:00Z"},"tv_shows":{"watching":"2026-05-04T12:05:00Z","completed":"2026-05-04T12:10:00Z"},"anime":{"watching":"2026-05-04T12:15:00Z","completed":"2026-05-04T12:20:00Z"}}`,
		"/sync/all-items/movies/completed": `{"movies":[{"status":"completed","last_watched_at":"2026-05-04T12:00:00Z","movie":{"title":"Inception","year":2010,"ids":{"imdb":"tt1375666","tmdb":"27205"}}}]}`,
		"/sync/all-items/shows/watching":   `{"shows":[{"status":"watching","show":{"title":"Breaking Bad","year":2008,"ids":{"tvdb":"81189","tmdb":"1396","imdb":"tt0903747"}},"seasons":[{"number":1,"episodes":[{"number":1,"watched_at":"2026-05-04T13:00:00Z","ids":{"tvdb":"349232"}},{"number":2}]}]}]}`,
		"/sync/all-items/shows/completed":  `{"shows":[]}`,
		"/sync/all-items/anime/watching":   `{"anime":[{"status":"watching","show":{"title":"Anime","year":2020,"ids":{"tmdb":"1429"}},"seasons":[{"number":1,"episodes":[{"number":4,"tvdb":{"season":2,"episode":4},"watched_at":"2026-05-04T14:00:00Z"}]}]}]}`,
		"/sync/all-items/anime/completed":  `{"anime":[]}`,
	}}
	server, _ := newTestServer(t, fake)

	result := listAll(t, server, kindWatched, "", 100)
	if result.complete {
		t.Fatal("a watched read must not be a complete snapshot")
	}
	rows := rowsByKey(t, result.items)
	if len(rows) != 3 {
		t.Fatalf("rows = %#v, want 3", rows)
	}
	movie := rows["imdb:tt1375666"]
	if movie != (row{
		ProviderItemKey: "imdb:tt1375666", Kind: "movie", Title: "Inception", Year: 2010,
		IMDbID: "tt1375666", TMDBID: "27205", PlayCount: 1,
		LastWatchedAt: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
	}) {
		t.Fatalf("movie row = %#v", movie)
	}
	episode := rows["tvdb:349232"]
	if episode != (row{
		ProviderItemKey: "tvdb:349232", Kind: "episode", TVDBID: "349232",
		SeriesTitle: "Breaking Bad", SeriesYear: 2008, SeriesIMDbID: "tt0903747", SeriesTMDBID: "1396", SeriesTVDBID: "81189",
		SeasonNumber: 1, EpisodeNumber: 1, PlayCount: 1, LastWatchedAt: time.Date(2026, 5, 4, 13, 0, 0, 0, time.UTC),
	}) {
		t.Fatalf("episode row = %#v", episode)
	}
	anime, ok := rows["show:tmdb:1429:s2:e4"]
	if !ok || anime.SeasonNumber != 2 || anime.EpisodeNumber != 4 || anime.SeriesTMDBID != "1429" || anime.SeriesTitle != "Anime" {
		t.Fatalf("anime row = %#v, want nested TVDB season/episode fallback", anime)
	}
	for _, path := range []string{"/sync/all-items/movies/completed", "/sync/all-items/shows/watching", "/sync/all-items/shows/completed", "/sync/all-items/anime/watching", "/sync/all-items/anime/completed"} {
		if fake.count(path) != 1 {
			t.Fatalf("%s read %d times, want once; requests %v", path, fake.count(path), fake.requests())
		}
	}
	for _, uri := range fake.requests() {
		if containsDateFrom(uri) {
			t.Fatalf("first traversal sent date_from: %s", uri)
		}
	}
	want := map[string]string{
		cursorInboundMoviesCompleted: "2026-05-04T12:00:00Z",
		cursorInboundShowsWatching:   "2026-05-04T12:05:00Z",
		cursorInboundShowsCompleted:  "2026-05-04T12:10:00Z",
		cursorInboundAnimeWatching:   "2026-05-04T12:15:00Z",
		cursorInboundAnimeCompleted:  "2026-05-04T12:20:00Z",
	}
	if got := decodedCursor(t, result.cursor); !reflect.DeepEqual(got, want) {
		t.Fatalf("cursor = %v, want %v", got, want)
	}
}

func containsDateFrom(uri string) bool {
	return strings.Contains(uri, "date_from=")
}

func TestListWatchedSkipsUnchangedActivityCursors(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{"/sync/activities": watchedActivitiesFixture}}
	server, _ := newTestServer(t, fake)
	cursor := encodedCursor(t, map[string]string{
		cursorInboundMoviesCompleted: "2026-05-04T12:00:00Z",
		cursorInboundShowsWatching:   "2026-05-04T12:05:00Z",
		cursorInboundShowsCompleted:  "2026-05-04T12:10:00Z",
		cursorInboundAnimeWatching:   "2026-05-04T12:15:00Z",
		cursorInboundAnimeCompleted:  "2026-05-04T12:20:00Z",
	})
	result := listAll(t, server, kindWatched, cursor, 100)
	if len(result.items) != 0 || len(fake.requests()) != 1 {
		t.Fatalf("items = %v requests = %v, want only the activities read", result.items, fake.requests())
	}
	if !reflect.DeepEqual(decodedCursor(t, result.cursor), decodedCursor(t, cursor)) {
		t.Fatalf("cursor = %s, want it unchanged", result.cursor)
	}
}

func TestListWatchedReadsChangedListsFromTheirCursor(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities": `{
			"movies":{"completed":"2026-05-04T12:00:00Z","removed_from_list":"2026-05-03T00:00:00Z"},
			"tv_shows":{"watching":"2026-05-05T09:00:00Z","completed":"2026-05-04T12:10:00Z"},
			"anime":{"watching":"","completed":"2026-05-04T12:20:00Z"}
		}`,
		"/sync/all-items/shows/watching": `{"shows":[]}`,
	}}
	server, _ := newTestServer(t, fake)
	cursor := encodedCursor(t, map[string]string{
		cursorInboundMoviesCompleted: "2026-05-04T12:00:00Z",
		cursorInboundShowsWatching:   "2026-05-04T12:05:00+00:00",
		cursorInboundShowsCompleted:  "2026-05-04T12:10:00Z",
		// A list whose activity is now empty is skipped, as before.
		cursorInboundAnimeWatching:  "2026-05-04T12:15:00Z",
		cursorInboundAnimeCompleted: "2026-05-04T12:20:00Z",
	})
	result := listAll(t, server, kindWatched, cursor, 100)
	requests := fake.requests()
	want := []string{
		"/sync/activities",
		"/sync/all-items/shows/watching?extended=full&episode_watched_at=yes&date_from=2026-05-04T12%3A05%3A00%2B00%3A00",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %v, want %v", requests, want)
	}
	got := decodedCursor(t, result.cursor)
	if got[cursorInboundShowsWatching] != "2026-05-05T09:00:00Z" || got[cursorInboundAnimeWatching] != "2026-05-04T12:15:00Z" ||
		got[cursorRemovedMovies] != "2026-05-03T00:00:00Z" {
		t.Fatalf("cursor = %v", got)
	}
}

func TestListWatchedWarnsWhenSimklRemovedTitlesFromAList(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities": `{
			"movies":{"completed":"2026-05-04T12:00:00Z","removed_from_list":"2026-05-03T00:00:00Z"},
			"tv_shows":{"completed":"2026-05-04T12:10:00Z","removed_from_list":"2026-05-02T00:00:00Z"},
			"anime":{"completed":"2026-05-04T12:20:00Z","removed_from_list":null}
		}`,
		"/sync/all-items/shows/watching": `{}`,
		"/sync/all-items/anime/watching": `{}`,
	}}
	server, _ := newTestServer(t, fake)
	cursor := map[string]string{
		cursorInboundMoviesCompleted: "2026-05-04T12:00:00Z",
		cursorInboundShowsCompleted:  "2026-05-04T12:10:00Z",
		cursorInboundAnimeCompleted:  "2026-05-04T12:20:00Z",
		cursorRemovedMovies:          "2026-05-01T00:00:00Z",
		cursorRemovedShows:           "2026-05-02T00:00:00Z",
	}
	result := listAll(t, server, kindWatched, encodedCursor(t, cursor), 100)
	if want := []string{warnRemovedFromList}; !reflect.DeepEqual(result.warnings, want) {
		t.Fatalf("warnings = %q, want %q for the movie list only", result.warnings, want)
	}
	if got := decodedCursor(t, result.cursor); got[cursorRemovedMovies] != "2026-05-03T00:00:00Z" || got[cursorRemovedShows] != "2026-05-02T00:00:00Z" {
		t.Fatalf("cursor = %v", got)
	}

	again := listAll(t, server, kindWatched, result.cursor, 100)
	if len(again.warnings) != 0 {
		t.Fatalf("warnings = %q, want none once the stamp is recorded", again.warnings)
	}
}

func TestListWatchedAndProgressWarnAboutTitlesWithoutIDs(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities": `{"movies":{"completed":"a","playback":"b"},"tv_shows":{"watching":"c","playback":"d"}}`,
		"/sync/all-items/movies/completed": `{"movies":[
			{"status":"completed","last_watched_at":"2026-05-04T12:00:00Z","movie":{"title":"No IDs","ids":{}}},
			{"status":"completed","last_watched_at":"2026-05-04T12:00:00Z","movie":{"title":"No IDs either","ids":{"slug":"x"}}},
			{"status":"completed","last_watched_at":"2026-05-04T12:00:00Z","movie":{"title":"Heat","ids":{"imdb":"tt0113277"}}}
		]}`,
		"/sync/all-items/shows/watching":  `{"shows":[{"status":"watching","show":{"title":"No IDs","ids":{}},"seasons":[{"number":1,"episodes":[{"number":1,"watched_at":"2026-05-04T13:00:00Z"}]}]}]}`,
		"/sync/all-items/shows/completed": `{}`,
		"/sync/all-items/anime/watching":  `{}`,
		"/sync/all-items/anime/completed": `{}`,
		"/sync/playback/movies":           `[{"type":"movie","progress":40,"paused_at":"2026-05-04T12:00:00Z","movie":{"title":"No IDs","ids":{}}}]`,
		"/sync/playback/episodes":         `[{"type":"episode","progress":20,"paused_at":"2026-05-04T12:00:00Z","show":{"title":"No IDs","ids":{}},"episode":{"season":1,"number":2}}]`,
	}}
	server, _ := newTestServer(t, fake)

	watched := listAll(t, server, kindWatched, "", 100)
	want := []string{warnWatchedEpisodeNoID, warnWatchedMovieNoID + " (2 items)"}
	if !reflect.DeepEqual(watched.warnings, want) || len(watched.items) != 1 {
		t.Fatalf("warnings = %q items = %d, want %q and one row", watched.warnings, len(watched.items), want)
	}
	progress := listAll(t, server, kindProgress, "", 100)
	want = []string{warnPlaybackEpisodeNoID, warnPlaybackMovieNoID}
	if !reflect.DeepEqual(progress.warnings, want) || len(progress.items) != 0 {
		t.Fatalf("warnings = %q items = %d, want %q and no rows", progress.warnings, len(progress.items), want)
	}
}

func TestListWatchedDatesCompletedShowsByTheShowWhenEpisodesHaveNoTime(t *testing.T) {
	show := func(status string) string {
		return `{"status":"` + status + `","last_watched_at":"2026-01-02T03:04:05Z","show":{"title":"S","year":2001,"ids":{"tvdb":"1"}},"seasons":[{"number":1,"episodes":[{"number":1}]}]}`
	}
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities":                 watchedActivitiesFixture,
		"/sync/all-items/movies/completed": `{"movies":[{"status":"plantowatch","last_watched_at":"2026-01-01T00:00:00Z","movie":{"ids":{"imdb":"tt1"}}},{"status":"completed","movie":{"ids":{"imdb":"tt2"}}}]}`,
		"/sync/all-items/shows/watching":   `{"shows":[` + show("watching") + `]}`,
		"/sync/all-items/shows/completed":  `{"shows":[` + show("completed") + `]}`,
		"/sync/all-items/anime/watching":   `{}`,
		"/sync/all-items/anime/completed":  `{}`,
	}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindWatched, "", 100)
	if len(result.items) != 1 {
		t.Fatalf("items = %v, want only the completed show's episode", hostRows(result.items))
	}
	if got := hostRow(result.items[0]); got.ProviderItemKey != "show:tvdb:1:s1:e1" || !got.LastWatchedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("row = %#v", got)
	}
}

func TestListProgressMapsPlaybackRows(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities": `{
			"movies":{"playback":"2026-05-04T12:00:00Z"},
			"tv_shows":{"playback":"2026-05-04T12:05:00Z"},
			"anime":{"playback":"2026-05-04T12:10:00Z"}
		}`,
		"/sync/playback/movies":   `[{"id":123,"type":"movie","progress":45.5,"paused_at":"2026-05-04T12:00:00Z","movie":{"title":"Inception","year":2010,"ids":{"imdb":"tt1375666","tmdb":27205}}}]`,
		"/sync/playback/episodes": `[{"id":124,"type":"episode","progress":12.5,"paused_at":"2026-05-04T12:02:00Z","show":{"title":"Breaking Bad","year":2008,"ids":{"tvdb":81189}},"episode":{"title":"Pilot","season":1,"number":1,"tvdb_season":2,"tvdb_number":4}}]`,
	}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindProgress, "", 100)
	rows := rowsByKey(t, result.items)
	if movie := rows["imdb:tt1375666"]; movie != (row{
		ProviderItemKey: "imdb:tt1375666", Kind: "movie", Title: "Inception", Year: 2010, IMDbID: "tt1375666", TMDBID: "27205",
		ProgressPercent: 45.5, PausedAt: time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC),
	}) {
		t.Fatalf("movie progress row = %#v", movie)
	}
	if episode := rows["show:tvdb:81189:s2:e4"]; episode != (row{
		ProviderItemKey: "show:tvdb:81189:s2:e4", Kind: "episode", Title: "Pilot",
		SeriesTitle: "Breaking Bad", SeriesYear: 2008, SeriesTVDBID: "81189", SeasonNumber: 2, EpisodeNumber: 4,
		ProgressPercent: 12.5, PausedAt: time.Date(2026, 5, 4, 12, 2, 0, 0, time.UTC),
	}) {
		t.Fatalf("episode progress row = %#v", episode)
	}
	if fake.count("/sync/playback/movies") != 1 || fake.count("/sync/playback/episodes") != 1 {
		t.Fatalf("typed playback endpoints were not fetched once: %v", fake.requests())
	}
	got := decodedCursor(t, result.cursor)
	if got[cursorProgressMovies] != "2026-05-04T12:00:00Z" || got[cursorProgressShows] != "2026-05-04T12:05:00Z" {
		t.Fatalf("cursor = %v", got)
	}
	if _, ok := got[cursorProgressAnime]; ok {
		t.Fatalf("cursor = %v; the anime stamp waits for an anime playback row", got)
	}
}

func TestListProgressSkipsUnchangedPlaybackActivities(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{"/sync/activities": `{
		"movies":{"playback":"2026-05-04T12:00:00Z"},
		"tv_shows":{"playback":"2026-05-04T12:05:00Z"},
		"anime":{"playback":"2026-05-04T12:10:00Z"}
	}`}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindProgress, encodedCursor(t, map[string]string{
		cursorProgressMovies: "2026-05-04T12:00:00Z",
		cursorProgressShows:  "2026-05-04T12:05:00Z",
		cursorProgressAnime:  "2026-05-04T12:10:00Z",
	}), 100)
	if len(result.items) != 0 || len(fake.requests()) != 1 {
		t.Fatalf("items = %v requests = %v, want skipped playback", result.items, fake.requests())
	}
}

func TestListProgressReadsEpisodesFromTheOlderStampAndRecordsAnimeWithAnimeRows(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities": `{
			"movies":{"playback":"2026-05-04T12:00:00Z"},
			"tv_shows":{"playback":"2026-05-06T00:00:00Z"},
			"anime":{"playback":"2026-05-07T00:00:00Z"}
		}`,
		"/sync/playback/episodes": `[{"type":"anime","progress":100,"paused_at":"2026-05-07T00:00:00Z","anime":{"title":"A","ids":{"tvdb":7}},"episode":{"season":1,"number":2}}]`,
	}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindProgress, encodedCursor(t, map[string]string{
		cursorProgressMovies: "2026-05-04T12:00:00Z",
		cursorProgressShows:  "2026-05-05T00:00:00Z",
		cursorProgressAnime:  "2026-05-01T00:00:00Z",
	}), 100)
	want := []string{"/sync/activities", "/sync/playback/episodes?date_from=2026-05-01T00%3A00%3A00Z"}
	if !reflect.DeepEqual(fake.requests(), want) {
		t.Fatalf("requests = %v, want %v", fake.requests(), want)
	}
	got := decodedCursor(t, result.cursor)
	if got[cursorProgressShows] != "2026-05-06T00:00:00Z" || got[cursorProgressAnime] != "2026-05-07T00:00:00Z" {
		t.Fatalf("cursor = %v", got)
	}
	if len(result.items) != 1 {
		t.Fatalf("items = %v", hostRows(result.items))
	}
	if anime := hostRow(result.items[0]); anime.ProviderItemKey != "show:tvdb:7:s1:e2" || anime.ProgressPercent != maxProgressPercent {
		t.Fatalf("anime row = %#v, want progress kept below 100", anime)
	}
}

func TestListWatchlistIsACompleteSnapshotOfMoviesShowsAndAnime(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/all-items/movies/plantowatch": `{"movies":[{"status":"plantowatch","movie":{"title":"Heat","year":1995,"ids":{"simkl":1,"imdb":"tt0113277","tmdb":"949"}}},{"movie":{"title":"No IDs","ids":{}}}]}`,
		"/sync/all-items/shows/plantowatch":  `{"shows":[{"show":{"title":"The Wire","year":2002,"ids":{"imdb":"tt0306414","tvdb":"79126","tmdb":1438}}}]}`,
		"/sync/all-items/anime/plantowatch":  `{"anime":[{"show":{"title":"Cowboy Bebop","year":1998,"ids":{"simkl":37089,"tmdb":"30991"}}}]}`,
	}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindWatchlist, "", 100)
	if !result.complete || result.cursor != "" {
		t.Fatalf("complete = %v cursor = %q, want an authoritative snapshot without a cursor", result.complete, result.cursor)
	}
	rows := rowsByKey(t, result.items)
	if len(rows) != 3 {
		t.Fatalf("rows = %v", rows)
	}
	if movie := rows["imdb:tt0113277"]; movie != (row{ProviderItemKey: "imdb:tt0113277", Kind: "movie", Title: "Heat", Year: 1995, IMDbID: "tt0113277", TMDBID: "949"}) {
		t.Fatalf("movie = %#v", movie)
	}
	if show := rows["tvdb:79126"]; show != (row{ProviderItemKey: "tvdb:79126", Kind: "series", Title: "The Wire", Year: 2002, IMDbID: "tt0306414", TMDBID: "1438", TVDBID: "79126"}) {
		t.Fatalf("show = %#v", show)
	}
	if anime := rows["tmdb:30991"]; anime.Kind != "series" || anime.Title != "Cowboy Bebop" {
		t.Fatalf("anime = %#v", anime)
	}
	for _, state := range result.items {
		if state.GetWatchlist() == nil || state.GetWatchlist().GetListedAt() != nil || state.GetWatchlist().GetRemoved() {
			t.Fatalf("watchlist state = %v, want listed without a time", state.GetWatchlist())
		}
	}
	for _, uri := range fake.requests() {
		if containsDateFrom(uri) {
			t.Fatalf("watchlist read sent date_from: %s", uri)
		}
	}
}

func TestListFavoritesIsUnsupported(t *testing.T) {
	server, _ := newTestServer(t, &fakeSimkl{t: t})
	_, fault := listAllWithFault(t, server, pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_FAVORITE, "", 100)
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", fault)
	}
}

func TestUnreadableCursorReadsEverything(t *testing.T) {
	fake := &fakeSimkl{t: t, bodies: map[string]string{
		"/sync/activities":        `{"movies":{"playback":"2026-05-04T12:00:00Z"}}`,
		"/sync/playback/movies":   `[]`,
		"/sync/playback/episodes": `[]`,
	}}
	server, _ := newTestServer(t, fake)
	result := listAll(t, server, kindProgress, "not-json", 100)
	if fake.count("/sync/playback/movies") != 1 || containsDateFrom(fake.requests()[1]) {
		t.Fatalf("requests = %v, want a full read", fake.requests())
	}
	if decodedCursor(t, result.cursor)[cursorProgressMovies] != "2026-05-04T12:00:00Z" {
		t.Fatalf("cursor = %q", result.cursor)
	}
}

func TestProviderItemKeysMatchTheBuiltInProvider(t *testing.T) {
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"movie prefers imdb", movieKey(simklIDs{Simkl: 1, IMDb: "tt1", TMDB: 2, TVDB: 3}), "imdb:tt1"},
		{"movie falls back to tmdb", movieKey(simklIDs{Simkl: 1, TMDB: 2, TVDB: 3}), "tmdb:2"},
		{"movie falls back to tvdb", movieKey(simklIDs{Simkl: 1, TVDB: 3}), "tvdb:3"},
		{"movie falls back to simkl", movieKey(simklIDs{Simkl: 1}), "simkl:1"},
		{"movie without ids", movieKey(simklIDs{Slug: "x"}), ""},
		{"show prefers tvdb", showKey(simklIDs{Simkl: 1, IMDb: "tt1", TMDB: 2, TVDB: 3}), "tvdb:3"},
		{"show falls back to tmdb", showKey(simklIDs{Simkl: 1, IMDb: "tt1", TMDB: 2}), "tmdb:2"},
		{"show falls back to imdb", showKey(simklIDs{Simkl: 1, IMDb: "tt1"}), "imdb:tt1"},
		{"show falls back to simkl", showKey(simklIDs{Simkl: 1}), "simkl:1"},
		{"episode tvdb", episodeKey(simklIDs{TVDB: 9}, 1, 2, simklIDs{TVDB: 5, TMDB: 6, Simkl: 7}), "tvdb:5"},
		{"episode tmdb", episodeKey(simklIDs{TVDB: 9}, 1, 2, simklIDs{TMDB: 6, Simkl: 7}), "tmdb:6"},
		{"episode simkl", episodeKey(simklIDs{TVDB: 9}, 1, 2, simklIDs{Simkl: 7, IMDb: "tt9"}), "simkl:7"},
		{"episode by show tvdb", episodeKey(simklIDs{TVDB: 9, TMDB: 8, IMDb: "tt8"}, 1, 2, simklIDs{IMDb: "tt9"}), "show:tvdb:9:s1:e2"},
		{"episode by show tmdb", episodeKey(simklIDs{TMDB: 8, IMDb: "tt8"}, 0, 3, simklIDs{}), "show:tmdb:8:s0:e3"},
		{"episode by show imdb", episodeKey(simklIDs{IMDb: "tt8", Simkl: 4}, 2, 1, simklIDs{}), "show:imdb:tt8:s2:e1"},
		{"episode by show simkl only", episodeKey(simklIDs{Simkl: 4}, 2, 1, simklIDs{}), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: key = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestSimklIDsDecodeNumbersAndStrings(t *testing.T) {
	var ids simklIDs
	if err := json.Unmarshal([]byte(`{"simkl":"12","slug":"s","imdb":"tt1","tmdb":34,"tvdb":"56","mal":"9"}`), &ids); err != nil {
		t.Fatal(err)
	}
	if ids != (simklIDs{Simkl: 12, Slug: "s", IMDb: "tt1", TMDB: 34, TVDB: 56}) {
		t.Fatalf("ids = %#v", ids)
	}
}
