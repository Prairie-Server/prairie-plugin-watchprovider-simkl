package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

type recordedRequest struct {
	method        string
	path          string
	authorization string
	apiKey        string
	body          map[string]any
}

// recordingServer answers every request with status and records it.
func recordingServer(t *testing.T, status int, body string) (*Server, *[]recordedRequest) {
	t.Helper()
	var requests []recordedRequest
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := recordedRequest{method: r.Method, path: r.URL.Path, authorization: r.Header.Get("Authorization"), apiKey: r.Header.Get("simkl-api-key")}
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&request.body)
		}
		requests = append(requests, request)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return server, &requests
}

func TestScrobbleSendsEpisodeAndMoviePayloads(t *testing.T) {
	server, requests := recordingServer(t, http.StatusCreated, `{}`)
	episode := &pluginv1.WatchSyncEvent{
		EventId:         "scrobble:start:1",
		Operation:       pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		PositionSeconds: 30,
		DurationSeconds: 3000,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:         mediaEpisode,
			ExternalIds:       map[string]string{"tvdb": "349232"},
			SeriesExternalIds: map[string]string{"tvdb": "81189", "imdb": "tt0903747"},
			SeasonNumber:      1,
			EpisodeNumber:     2,
		},
	}
	movie := scrobbleStart()
	movie.EventId = "scrobble:pause:1"
	movie.Operation = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE

	results := resultsByID(applyEvents(t, server, episode, movie))
	for _, id := range []string{"scrobble:start:1", "scrobble:pause:1"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Fatalf("%s = %v", id, results[id])
		}
	}
	if len(*requests) != 2 {
		t.Fatalf("requests = %v", *requests)
	}
	start := (*requests)[0]
	if start.method != http.MethodPost || start.path != "/scrobble/start" || start.authorization != "Bearer "+testToken || start.apiKey != testClientID {
		t.Fatalf("start request = %+v", start)
	}
	wantEpisode := map[string]any{
		"progress": float64(1),
		"show":     map[string]any{"ids": map[string]any{"imdb": "tt0903747", "tvdb": float64(81189)}},
		"episode":  map[string]any{"season": float64(1), "number": float64(2), "ids": map[string]any{"tvdb": float64(349232)}},
	}
	if !reflect.DeepEqual(start.body, wantEpisode) {
		t.Fatalf("episode payload = %#v, want %#v", start.body, wantEpisode)
	}
	pause := (*requests)[1]
	wantMovie := map[string]any{"progress": float64(1), "movie": map[string]any{"ids": map[string]any{"imdb": "tt1375666"}}}
	if pause.path != "/scrobble/pause" || !reflect.DeepEqual(pause.body, wantMovie) {
		t.Fatalf("pause request = %+v", pause)
	}
}

func TestStopTreatsCompletedConflictAsNoChange(t *testing.T) {
	server, _ := recordingServer(t, http.StatusConflict, `{"error":"already_watched"}`)
	stop := scrobbleStart()
	stop.EventId = "stop"
	stop.Operation = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP
	stop.PositionSeconds, stop.DurationSeconds, stop.Completed = 5400, 6000, true
	if status := resultsByID(applyEvents(t, server, stop))["stop"].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE {
		t.Fatalf("completed stop conflict = %v, want no change", status)
	}
	stop.Completed = false
	if status := resultsByID(applyEvents(t, server, stop))["stop"].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
		t.Fatalf("incomplete stop conflict = %v, want rejected", status)
	}
}

func TestRevokedTokenIsAConnectionWideInvalidCredential(t *testing.T) {
	server, _ := recordingServer(t, http.StatusUnauthorized, `{"error":"user_token_failed","message":"token `+testToken+` revoked"}`)
	response := applyEvents(t, server, scrobbleStart())
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL || len(response.GetResults()) != 0 {
		t.Fatalf("response = %v, want an invalid credential fault on the response", response)
	}
	assertSafe(t, response.GetFault())

	_, fault := listAllWithFault(t, server, kindWatched, "", 100)
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("list fault = %v", fault)
	}
}

func TestUpstreamFaultsCarryNoSecrets(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusPreconditionFailed, http.StatusTeapot, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		server, _ := recordingServer(t, status, `{"error":"x","message":"`+testToken+` `+testClientID+`"}`)
		_, fault := listAllWithFault(t, server, kindWatched, "", 100)
		if fault == nil {
			t.Fatalf("HTTP %d: no fault", status)
		}
		assertSafe(t, fault)
	}
}

func TestScrobbleRejectsSeriesWithoutCallingSimkl(t *testing.T) {
	server, requests := recordingServer(t, http.StatusCreated, `{}`)
	event := scrobbleStart()
	event.Media.MediaType = mediaSeries
	if status := resultsByID(applyEvents(t, server, event))[event.GetEventId()].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || len(*requests) != 0 {
		t.Fatalf("status = %v requests = %v", status, *requests)
	}
}

func TestWatchlistChangesSendOneRequest(t *testing.T) {
	for _, tc := range []struct {
		operation pluginv1.WatchSyncOperation
		path      string
		to        any
	}{
		{pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST, "/sync/add-to-list", "plantowatch"},
		{pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST, "/sync/remove-from-list", nil},
	} {
		t.Run(tc.path, func(t *testing.T) {
			server, requests := recordingServer(t, http.StatusCreated, `{}`)
			results := resultsByID(applyEvents(t, server,
				movieEvent("movie", tc.operation, map[string]string{"imdb": "tt0113277", "tmdb": "949"}),
				func() *pluginv1.WatchSyncEvent {
					event := seriesEvent("series", tc.operation, nil)
					event.ProviderItemKey = "tvdb:79126"
					return event
				}(),
				seriesEvent("no-ids", tc.operation, nil),
				func() *pluginv1.WatchSyncEvent {
					event := movieEvent("episode", tc.operation, map[string]string{"tvdb": "1"})
					event.Media.MediaType = mediaEpisode
					return event
				}(),
			))
			if len(*requests) != 1 || (*requests)[0].path != tc.path {
				t.Fatalf("requests = %+v", *requests)
			}
			body := (*requests)[0].body
			movies := body["movies"].([]any)
			shows := body["shows"].([]any)
			if len(movies) != 1 || len(shows) != 1 {
				t.Fatalf("body = %#v", body)
			}
			movie, show := movies[0].(map[string]any), shows[0].(map[string]any)
			if movie["to"] != tc.to || show["to"] != tc.to ||
				!reflect.DeepEqual(movie["ids"], map[string]any{"imdb": "tt0113277", "tmdb": float64(949)}) ||
				!reflect.DeepEqual(show["ids"], map[string]any{"tvdb": float64(79126)}) {
				t.Fatalf("body = %#v", body)
			}
			for id, want := range map[string]pluginv1.WatchSyncApplyStatus{
				"movie":   pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED,
				"series":  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED,
				"no-ids":  pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED,
				"episode": pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED,
			} {
				if results[id].GetStatus() != want {
					t.Fatalf("%s = %v, want %v", id, results[id], want)
				}
			}
		})
	}
}

func TestUnsupportedOperationsAndMissingMediaAreRejected(t *testing.T) {
	server, requests := recordingServer(t, http.StatusCreated, `{}`)
	favorite := movieEvent("favorite", pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_FAVORITE, map[string]string{"imdb": "tt1"})
	bare := &pluginv1.WatchSyncEvent{EventId: "bare", Operation: opMarkWatched}
	results := resultsByID(applyEvents(t, server, favorite, bare, &pluginv1.WatchSyncEvent{}))
	if len(results) != 2 || len(*requests) != 0 {
		t.Fatalf("results = %v requests = %v", results, *requests)
	}
	for _, id := range []string{"favorite", "bare"} {
		if results[id].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED {
			t.Fatalf("%s = %v", id, results[id])
		}
	}
}

func TestOversizedResponseIsAPermanentFault(t *testing.T) {
	server, _ := recordingServer(t, http.StatusOK, `{"movies":[{"padding":"`+strings.Repeat("x", 512)+`"}]}`)
	server.simkl.maxResponse = 256
	_, fault := listAllWithFault(t, server, kindWatchlist, "", 100)
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
		t.Fatalf("fault = %v, want permanent", fault)
	}
}

func TestEventIDsAreAnsweredAsSent(t *testing.T) {
	server, _ := recordingServer(t, http.StatusCreated, `{}`)
	event := scrobbleStart()
	event.EventId = " spaced "
	response := applyEvents(t, server, event)
	if len(response.GetResults()) != 1 || response.GetResults()[0].GetEventId() != " spaced " ||
		response.GetResults()[0].GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("results = %v", response.GetResults())
	}
}

func TestShortDeadlineIsNotCutFurther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcDeadlineMargin/2)
	defer cancel()
	derived, stop := withRPCDeadline(ctx)
	defer stop()
	if derived.Err() != nil {
		t.Fatal("a deadline shorter than the margin must not expire at once")
	}
}
