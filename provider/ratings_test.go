package provider

import (
	"encoding/json"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	opSetRating    = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING
	opRemoveRating = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING
)

const ratingsActivitiesFixture = `{
	"movies":{"rated_at":"2026-05-04T12:00:00Z"},
	"tv_shows":{"rated_at":"2026-05-04T12:05:00Z"},
	"anime":{"rated_at":"2026-05-04T12:10:00Z"}
}`

// ratingsServer answers /sync/activities with activities and each ratings read
// with the body lists holds for its type, recording the ratings reads.
type ratingsServer struct {
	t          *testing.T
	activities string
	lists      map[string]string
	mu         sync.Mutex
	reads      []string
}

func (s *ratingsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/sync/activities" {
		writeJSON(s.t, w, s.activities)
		return
	}
	listType, filter, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/sync/ratings/"), "/")
	if r.Method != http.MethodGet || !ok {
		s.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		http.NotFound(w, r)
		return
	}
	s.mu.Lock()
	s.reads = append(s.reads, listType)
	s.mu.Unlock()
	if filter != simklEveryRating {
		s.t.Errorf("%s rating filter = %q, want %q", listType, filter, simklEveryRating)
	}
	if r.URL.RawQuery != "" {
		s.t.Errorf("%s query = %q, want a full read without date_from", listType, r.URL.RawQuery)
	}
	body, found := s.lists[listType]
	if !found {
		body = `{}`
	}
	writeJSON(s.t, w, body)
}

func (s *ratingsServer) readTypes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	types := append([]string(nil), s.reads...)
	sort.Strings(types)
	return types
}

func listRatingsFrom(t *testing.T, fake *ratingsServer, cursor map[string]string) traversal {
	t.Helper()
	server, _ := newTestServer(t, fake)
	encoded := ""
	if cursor != nil {
		encoded = encodedCursor(t, cursor)
	}
	return listAll(t, server, kindRating, encoded, 100)
}

func TestListRatingsSkipsUnchangedRatedAt(t *testing.T) {
	// Movies and shows match their cursors; anime was never rated (null).
	fake := &ratingsServer{t: t, activities: `{
		"movies":{"rated_at":"2026-05-04T12:00:00Z"},
		"tv_shows":{"rated_at":"2026-05-04T12:05:00Z"},
		"anime":{"rated_at":null}
	}`}
	cursor := map[string]string{
		cursorRatingsMovies: "2026-05-04T12:00:00Z",
		cursorRatingsShows:  "2026-05-04T12:05:00Z",
	}
	result := listRatingsFrom(t, fake, cursor)
	if reads := fake.readTypes(); len(reads) != 0 {
		t.Fatalf("ratings reads = %v, want none", reads)
	}
	if len(result.items) != 0 || result.complete || !reflect.DeepEqual(decodedCursor(t, result.cursor), cursor) {
		t.Fatalf("result = %+v, want no rows, no snapshot, and the same cursor", result)
	}
}

func TestListRatingsReadsEveryListWhenAnyChanged(t *testing.T) {
	const old = "2026-05-01T00:00:00Z"
	current := map[string]string{
		cursorRatingsMovies: "2026-05-04T12:00:00Z",
		cursorRatingsShows:  "2026-05-04T12:05:00Z",
		cursorRatingsAnime:  "2026-05-04T12:10:00Z",
	}
	withOld := func(key string) map[string]string {
		cursor := cloneMap(current)
		cursor[key] = old
		return cursor
	}
	for _, tc := range []struct {
		name   string
		cursor map[string]string
	}{
		{name: "movies changed", cursor: withOld(cursorRatingsMovies)},
		{name: "shows changed", cursor: withOld(cursorRatingsShows)},
		{name: "anime changed", cursor: withOld(cursorRatingsAnime)},
		{name: "first read", cursor: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &ratingsServer{t: t, activities: ratingsActivitiesFixture}
			result := listRatingsFrom(t, fake, tc.cursor)
			if reads := fake.readTypes(); !reflect.DeepEqual(reads, []string{"anime", "movies", "shows"}) {
				t.Fatalf("ratings reads = %v, want every list", reads)
			}
			if !result.complete {
				t.Fatal("a clean read of every list must be a complete snapshot")
			}
			if got := decodedCursor(t, result.cursor); !reflect.DeepEqual(got, current) {
				t.Fatalf("cursor = %v, want %v", got, current)
			}
		})
	}
}

func TestListRatingsMapsMoviesShowsAndAnime(t *testing.T) {
	fake := &ratingsServer{t: t, activities: ratingsActivitiesFixture, lists: map[string]string{
		"movies": `{"movies":[
			{"user_rating":7,"user_rated_at":"2026-03-01T10:00:00.000Z","status":"completed","movie":{"title":"Heat","year":1995,"ids":{"simkl":1,"imdb":"tt0113277","tmdb":"949"}}},
			{"user_rating":null,"user_rated_at":null,"movie":{"title":"Unrated","year":2001,"ids":{"imdb":"tt0000002"}}},
			{"user_rating":4,"movie":{"title":"No IDs","year":2002,"ids":{}}}
		]}`,
		"shows": `{"shows":[
			{"user_rating":10,"user_rated_at":"2026-03-02T10:00:00Z","status":"watching","show":{"title":"The Wire","year":2002,"ids":{"simkl":2,"imdb":"tt0306414","tvdb":"79126"}}}
		]}`,
		"anime": `{"anime":[
			{"user_rating":9,"user_rated_at":"2026-03-03T10:00:00Z","anime_type":"tv","show":{"title":"Cowboy Bebop","year":1998,"ids":{"simkl":37089,"mal":"1","tvdb":"76885"}}},
			{"user_rating":8,"user_rated_at":"2026-03-04T10:00:00Z","anime_type":"movie","show":{"title":"Akira","year":1988,"ids":{"simkl":3,"imdb":"tt0094625","tmdb":"149"}}}
		]}`,
	}}
	result := listRatingsFrom(t, fake, nil)
	rows := rowsByKey(t, result.items)
	if len(rows) != 4 {
		t.Fatalf("rows = %#v, want the four rated items", rows)
	}
	if movie := rows["imdb:tt0113277"]; movie != (row{
		ProviderItemKey: "imdb:tt0113277", Kind: "movie", Title: "Heat", Year: 1995, IMDbID: "tt0113277", TMDBID: "949",
		Rating: 7, RatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
	}) {
		t.Fatalf("movie row = %#v", movie)
	}
	if show := rows["tvdb:79126"]; show.Kind != "series" || show.Rating != 10 || show.IMDbID != "tt0306414" || show.Title != "The Wire" {
		t.Fatalf("show row = %#v", show)
	}
	if anime := rows["tvdb:76885"]; anime.Kind != "series" || anime.Rating != 9 || anime.Title != "Cowboy Bebop" {
		t.Fatalf("anime series row = %#v, want a series", anime)
	}
	if animeMovie := rows["imdb:tt0094625"]; animeMovie.Kind != "movie" || animeMovie.Rating != 8 || animeMovie.TMDBID != "149" {
		t.Fatalf("anime movie row = %#v, want a movie", animeMovie)
	}
	if result.complete {
		t.Fatal("a rated movie without ids was skipped, so the read is not a complete snapshot")
	}
}

func TestRatingStatesFromListClassifiesAnimeByType(t *testing.T) {
	// Every entry carries a TMDB id, which for a movie-like anime entry can
	// be a TMDB movie id.
	const idsJSON = `{"simkl":7,"imdb":"tt0000007","tmdb":"550","tvdb":"76885","mal":"1"}`
	for _, tc := range []struct {
		name      string
		animeType string // "" leaves the field out
		wantKind  string
		wantTMDB  string
		wantKey   string
	}{
		{name: "movie", animeType: `"movie"`, wantKind: "movie", wantTMDB: "550", wantKey: "imdb:tt0000007"},
		{name: "movie uppercase", animeType: `"MOVIE"`, wantKind: "movie", wantTMDB: "550", wantKey: "imdb:tt0000007"},
		{name: "tv", animeType: `"tv"`, wantKind: "series", wantTMDB: "550", wantKey: "tvdb:76885"},
		{name: "ova", animeType: `"ova"`, wantKind: "series", wantKey: "tvdb:76885"},
		{name: "ona", animeType: `"ona"`, wantKind: "series", wantKey: "tvdb:76885"},
		{name: "special", animeType: `"special"`, wantKind: "series", wantKey: "tvdb:76885"},
		{name: "music video", animeType: `"music video"`, wantKind: "series", wantKey: "tvdb:76885"},
		{name: "null", animeType: `null`, wantKind: "series", wantKey: "tvdb:76885"},
		{name: "missing", wantKind: "series", wantKey: "tvdb:76885"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typeField := ""
			if tc.animeType != "" {
				typeField = `"anime_type":` + tc.animeType + `,`
			}
			var list simklRatingsList
			body := `{"anime":[{"user_rating":8,` + typeField + `"show":{"title":"T","year":2000,"ids":` + idsJSON + `}}]}`
			if err := json.Unmarshal([]byte(body), &list); err != nil {
				t.Fatalf("decode: %v", err)
			}
			states, untyped, skipped, _ := ratingStatesFromList(list, simklTypeAnime)
			if len(states) != 1 || len(skipped) != 0 {
				t.Fatalf("states = %v skipped = %v, want one row", states, skipped)
			}
			if wantUntyped := tc.wantKind == "series" && tc.wantTMDB == ""; untyped != wantUntyped {
				t.Fatalf("untyped = %v, want %v", untyped, wantUntyped)
			}
			got := hostRow(states[0])
			if got.Kind != tc.wantKind || got.TMDBID != tc.wantTMDB || got.ProviderItemKey != tc.wantKey {
				t.Fatalf("row kind %q tmdb %q key %q, want %q %q %q", got.Kind, got.TMDBID, got.ProviderItemKey, tc.wantKind, tc.wantTMDB, tc.wantKey)
			}
			if got.IMDbID != "tt0000007" || got.TVDBID != "76885" {
				t.Fatalf("row imdb %q tvdb %q, want both kept", got.IMDbID, got.TVDBID)
			}
		})
	}
}

func TestRatingStatesFromListUntypedAnimeWithOnlyTMDBHasNoExternalID(t *testing.T) {
	var list simklRatingsList
	if err := json.Unmarshal([]byte(`{"anime":[
		{"user_rating":7,"anime_type":"ova","show":{"title":"OVA","ids":{"simkl":9,"tmdb":"550"}}},
		{"user_rating":7,"show":{"title":"Untyped","ids":{"tmdb":"551"}}}
	]}`), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	states, _, skipped, warnings := ratingStatesFromList(list, simklTypeAnime)
	// The OVA keeps only its Simkl id; the untyped entry has no id left.
	if len(states) != 1 {
		t.Fatalf("states = %v", states)
	}
	if got := hostRow(states[0]); got.ProviderItemKey != "simkl:9" || got.Kind != "series" || got.TMDBID != "" || got.IMDbID != "" || got.TVDBID != "" {
		t.Fatalf("row = %#v, want one series row keyed by its Simkl id without a TMDB id", got)
	}
	if !reflect.DeepEqual(skipped, map[pluginv1.WatchSyncMediaType]bool{pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES: true}) {
		t.Fatalf("skipped kinds = %v, want series for the untyped entry", skipped)
	}
	if want := []string{"simkl series rating skipped because it has no usable id; skipped movie and series rating removals"}; !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings = %v, want %v", warnings, want)
	}
}

func TestRatingStatesRoundAndClampToTheContractScale(t *testing.T) {
	var list simklRatingsList
	if err := json.Unmarshal([]byte(`{"movies":[
		{"user_rating":7.5,"movie":{"ids":{"imdb":"tt1"}}},
		{"user_rating":0,"movie":{"ids":{"imdb":"tt2"}}},
		{"user_rating":11,"movie":{"ids":{"imdb":"tt3"}}}
	]}`), &list); err != nil {
		t.Fatal(err)
	}
	states, _, _, _ := ratingStatesFromList(list, simklTypeMovies)
	got := map[string]int32{}
	for _, state := range states {
		got[state.GetProviderItemKey()] = state.GetRating().GetRating()
	}
	if want := map[string]int32{"imdb:tt1": 8, "imdb:tt2": 1, "imdb:tt3": 10}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ratings = %v, want %v", got, want)
	}
}

func TestListRatingsIsNotASnapshotWhenAnyKindIsUnproven(t *testing.T) {
	for _, tc := range []struct {
		name         string
		lists        map[string]string
		wantComplete bool
	}{
		{
			// The documented ratings read carries no anime_type, and such an
			// entry may be an anime movie.
			name:  "untyped anime",
			lists: map[string]string{"anime": `{"anime":[{"user_rating":8,"show":{"title":"Akira","year":1988,"ids":{"simkl":3,"imdb":"tt0094625"}}}]}`},
		},
		{
			name:  "untyped anime with only a tmdb id",
			lists: map[string]string{"anime": `{"anime":[{"user_rating":7,"show":{"title":"Untyped","ids":{"tmdb":"551"}}}]}`},
		},
		{
			name:  "show with no ids",
			lists: map[string]string{"shows": `{"shows":[{"user_rating":6,"show":{"title":"No IDs","year":2003,"ids":{}}}]}`},
		},
		{
			name:  "typed anime movie with no ids",
			lists: map[string]string{"anime": `{"anime":[{"user_rating":6,"anime_type":"movie","show":{"title":"No IDs","ids":{}}}]}`},
		},
		{
			name:  "movie with no ids",
			lists: map[string]string{"movies": `{"movies":[{"user_rating":4,"movie":{"title":"No IDs","year":2002,"ids":{}}}]}`},
		},
		{
			name:         "typed anime and clean lists",
			lists:        map[string]string{"anime": `{"anime":[{"user_rating":8,"anime_type":"tv","show":{"ids":{"tvdb":"1"}}}]}`},
			wantComplete: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := listRatingsFrom(t, &ratingsServer{t: t, activities: ratingsActivitiesFixture, lists: tc.lists}, nil)
			if result.complete != tc.wantComplete {
				t.Fatalf("complete = %v, want %v", result.complete, tc.wantComplete)
			}
		})
	}
}

func TestListRatingsWarnsAboutRatingsItCannotProve(t *testing.T) {
	fake := &ratingsServer{t: t, activities: ratingsActivitiesFixture, lists: map[string]string{
		"movies": `{"movies":[
			{"user_rating":4,"movie":{"title":"No IDs","ids":{}}},
			{"user_rating":5,"movie":{"title":"No IDs either","ids":{}}},
			{"user_rating":6,"movie":{"title":"Heat","ids":{"imdb":"tt0113277"}}}
		]}`,
		"shows": `{"shows":[{"user_rating":6,"show":{"title":"No IDs","ids":{}}}]}`,
		"anime": `{"anime":[{"user_rating":8,"show":{"title":"Akira","year":1988,"ids":{"simkl":3,"imdb":"tt0094625"}}}]}`,
	}}
	result := listRatingsFrom(t, fake, nil)
	want := []string{
		"simkl movie rating skipped because it has no usable id; skipped movie and series rating removals (2 items)",
		"simkl returned rated anime without a movie or tv type; skipped movie and series rating removals",
		"simkl series rating skipped because it has no usable id; skipped movie and series rating removals",
	}
	if !reflect.DeepEqual(result.warnings, want) {
		t.Fatalf("warnings = %q, want %q", result.warnings, want)
	}
	if result.complete || len(result.items) != 2 {
		t.Fatalf("result = %+v, want the two usable ratings in an incomplete read", result)
	}

	clean := listRatingsFrom(t, &ratingsServer{t: t, activities: ratingsActivitiesFixture}, nil)
	if len(clean.warnings) != 0 || !clean.complete {
		t.Fatalf("clean read = %+v, want a complete read without warnings", clean)
	}
}

// ratingsWriteServer records one POST body and answers with response.
func ratingsWriteServer(t *testing.T, wantPath, response string, got any, calls *int) *Server {
	t.Helper()
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodPost || r.URL.Path != wantPath {
			t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, wantPath)
		}
		if err := json.NewDecoder(r.Body).Decode(got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		writeJSON(t, w, response)
	}))
	return server
}

func ratingEvent(id string, kind pluginv1.WatchSyncMediaType, operation pluginv1.WatchSyncOperation, rating int32, key string, ids map[string]string) *pluginv1.WatchSyncEvent {
	event := movieEvent(id, operation, ids)
	event.Media.MediaType = kind
	event.Rating = rating
	event.ProviderItemKey = key
	return event
}

const (
	mediaMovie   = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	mediaSeries  = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	mediaEpisode = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE
)

func TestSetRatingsSendsMoviesAndSeriesInOneBatch(t *testing.T) {
	var got map[string][]map[string]any
	var calls int
	server := ratingsWriteServer(t, "/sync/ratings", `{"added":{"movies":1,"shows":1},"not_found":{"movies":[],"shows":[]}}`, &got, &calls)
	// The host sends a movie rating only after the profile watched the movie,
	// so the plugin sends it like any other.
	movie := ratingEvent("m1", mediaMovie, opSetRating, 6, "imdb:tt0113277", map[string]string{"imdb": "tt0113277", "tmdb": "949"})
	movie.OccurredAt = timestamp(time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC))
	results := resultsByID(applyEvents(t, server,
		movie,
		// An agreed row for a removed media item carries only its key.
		ratingEvent("s1", mediaSeries, opSetRating, 8, "tvdb:79126", nil),
		ratingEvent("no-ids", mediaMovie, opSetRating, 4, "", nil),
		ratingEvent("episode", mediaEpisode, opSetRating, 4, "", map[string]string{"tvdb": "1"}),
		ratingEvent("out-of-range", mediaMovie, opSetRating, 11, "", map[string]string{"imdb": "tt1"}),
	))
	if calls != 1 {
		t.Fatalf("requests = %d, want one batch", calls)
	}
	movies, shows := got["movies"], got["shows"]
	if len(movies) != 1 || movies[0]["rating"] != float64(6) || movies[0]["rated_at"] != "2026-04-05T06:07:08Z" ||
		!reflect.DeepEqual(movies[0]["ids"], map[string]any{"imdb": "tt0113277", "tmdb": float64(949)}) {
		t.Fatalf("movies payload = %#v", movies)
	}
	if len(shows) != 1 || shows[0]["rating"] != float64(8) || shows[0]["rated_at"] != nil ||
		!reflect.DeepEqual(shows[0]["ids"], map[string]any{"tvdb": float64(79126)}) {
		t.Fatalf("shows payload = %#v", shows)
	}
	if len(got) != 2 {
		t.Fatalf("payload keys = %#v, want movies and shows only", got)
	}
	for _, id := range []string{"m1", "s1"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Fatalf("%s = %v", id, results[id])
		}
	}
	for _, id := range []string{"no-ids", "episode", "out-of-range"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Fatalf("%s = %v, want rejected", id, results[id])
		}
	}
}

func TestSetRatingsSkipsRequestWithoutUsableItems(t *testing.T) {
	var got any
	var calls int
	server := ratingsWriteServer(t, "/sync/ratings", `{}`, &got, &calls)
	response := applyEvents(t, server,
		ratingEvent("no-ids", mediaMovie, opSetRating, 4, "", nil),
		ratingEvent("no-ids-series", mediaSeries, opSetRating, 4, "", nil),
	)
	if calls != 0 || response.GetFault() != nil {
		t.Fatalf("calls = %d response = %v, want no request", calls, response)
	}
}

func TestSetRatingsMapsNotFoundByAnySharedIDOfTheSameKind(t *testing.T) {
	var got any
	var calls int
	// The movie echo carries only the TMDB id, the show echo only the IMDb id,
	// and a show echo's TMDB id equals a movie's TMDB id, which must not match.
	server := ratingsWriteServer(t, "/sync/ratings", `{
		"added":{"movies":1,"shows":0,"statuses":[]},
		"not_found":{
			"movies":[{"rating":6,"ids":{"tmdb":"949"},"type":"movie"}],
			"shows":[{"rating":8,"ids":{"imdb":"tt0306414"},"type":"show"},{"rating":8,"ids":{"tmdb":1438},"type":"show"}]
		}
	}`, &got, &calls)
	results := resultsByID(applyEvents(t, server,
		ratingEvent("m1", mediaMovie, opSetRating, 6, "imdb:tt0113277", map[string]string{"imdb": "tt0113277", "tmdb": "949"}),
		ratingEvent("s1", mediaSeries, opSetRating, 8, "tvdb:79126", map[string]string{"imdb": "tt0306414", "tvdb": "79126"}),
		ratingEvent("m2", mediaMovie, opSetRating, 10, "tmdb:1438", map[string]string{"tmdb": "1438"}),
	))
	for id, want := range map[string]pluginv1.WatchSyncApplyStatus{
		"m1": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED,
		"s1": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED,
		"m2": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED,
	} {
		if results[id].GetStatus() != want {
			t.Fatalf("%s = %v, want %v", id, results[id], want)
		}
	}
}

func TestSetRatingsReportsEachEventByItsOwnKind(t *testing.T) {
	var got any
	var calls int
	// A movie and a series share the key tmdb:550; only the series is echoed
	// as not found.
	server := ratingsWriteServer(t, "/sync/ratings", `{
		"added":{"movies":1,"shows":0},
		"not_found":{"movies":[],"shows":[{"rating":8,"ids":{"tmdb":550},"type":"show"}]}
	}`, &got, &calls)
	results := resultsByID(applyEvents(t, server,
		ratingEvent("movie", mediaMovie, opSetRating, 6, "tmdb:550", map[string]string{"tmdb": "550"}),
		ratingEvent("series", mediaSeries, opSetRating, 8, "tmdb:550", map[string]string{"tmdb": "550"}),
	))
	if results["movie"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED ||
		results["series"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("results = %v, want the movie applied and the series not found", results)
	}
}

func TestRemoveRatingsSendsIDsOnlyAndTreatsNotFoundAsCleared(t *testing.T) {
	var got map[string][]map[string]any
	var calls int
	server := ratingsWriteServer(t, "/sync/ratings/remove", `{
		"deleted":{"movies":1,"shows":0},
		"not_found":{"movies":[],"shows":[{"ids":{"tvdb":"79126"},"type":"show"}]}
	}`, &got, &calls)
	results := resultsByID(applyEvents(t, server,
		ratingEvent("m1", mediaMovie, opRemoveRating, 0, "tmdb:949", nil),
		ratingEvent("s1", mediaSeries, opRemoveRating, 0, "tvdb:79126", map[string]string{"imdb": "tt0306414", "tvdb": "79126"}),
		ratingEvent("no-ids", mediaSeries, opRemoveRating, 0, "", nil),
	))
	if calls != 1 {
		t.Fatalf("requests = %d, want one batch", calls)
	}
	if want := []map[string]any{{"ids": map[string]any{"tmdb": float64(949)}}}; !reflect.DeepEqual(got["movies"], want) {
		t.Fatalf("movies payload = %#v, want ids without a rating", got["movies"])
	}
	if want := []map[string]any{{"ids": map[string]any{"imdb": "tt0306414", "tvdb": float64(79126)}}}; !reflect.DeepEqual(got["shows"], want) {
		t.Fatalf("shows payload = %#v, want ids without a rating", got["shows"])
	}
	if results["m1"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED ||
		results["s1"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE ||
		results["no-ids"].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("results = %v", results)
	}
}
