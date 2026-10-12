package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// rewatchFake answers the watched reads with empty lists and records each
// history write's URI and body.
type rewatchFake struct {
	t      *testing.T
	mu     sync.Mutex
	writes []rewatchWrite
}

type rewatchWrite struct {
	uri  string
	body map[string]any
}

func (f *rewatchFake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/sync/all-items/"):
		writeJSON(f.t, w, `{}`)
	case r.Method == http.MethodPost && r.URL.Path == "/sync/history":
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			f.t.Errorf("decode body: %v", err)
		}
		f.writes = append(f.writes, rewatchWrite{uri: r.URL.RequestURI(), body: body})
		writeJSON(f.t, w, `{"added":{"movies":1},"not_found":{"movies":[],"shows":[],"episodes":[]}}`)
	default:
		f.t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
	}
}

func applyEventsWithSettings(t *testing.T, server *Server, settings map[string]string, events ...*pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyEventsResponse {
	t.Helper()
	auth := authContext()
	auth.ConnectionSettings = settings
	response, err := server.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{Context: auth, Events: events})
	if err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	return response
}

func TestMarkWatchedSendsRewatchesOnlyWhenTheProfileTurnedThemOn(t *testing.T) {
	at := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		settings map[string]string
		wantURI  string
	}{
		// An older Prairie server sends no connection settings.
		{name: "no settings", settings: nil, wantURI: "/sync/history"},
		{name: "off", settings: map[string]string{settingTrackRewatches: "false"}, wantURI: "/sync/history"},
		{name: "on", settings: map[string]string{settingTrackRewatches: "true"}, wantURI: "/sync/history?allow_rewatch=yes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &rewatchFake{t: t}
			server, _ := newTestServer(t, fake)
			response := applyEventsWithSettings(t, server, tc.settings,
				moviePlay("history-1", at, map[string]string{"imdb": "tt1375666"}))
			if status := resultsByID(response)["history-1"].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
				t.Fatalf("status = %v", status)
			}
			if len(fake.writes) != 1 || fake.writes[0].uri != tc.wantURI {
				t.Fatalf("writes = %+v, want one write to %s", fake.writes, tc.wantURI)
			}
		})
	}
}

func TestMarkWatchedSendsAPlayWithoutAWatchTimeWithoutTheRewatchFlag(t *testing.T) {
	fake := &rewatchFake{t: t}
	server, _ := newTestServer(t, fake)
	timed := moviePlay("timed", time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC), map[string]string{"imdb": "tt1375666"})
	untimed := moviePlay("untimed", time.Time{}, map[string]string{"imdb": "tt0816692"})
	untimed.OccurredAt = nil
	response := applyEventsWithSettings(t, server, map[string]string{settingTrackRewatches: "true"}, timed, untimed)
	for _, id := range []string{"timed", "untimed"} {
		if status := resultsByID(response)[id].GetStatus(); status != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
			t.Fatalf("%s status = %v", id, status)
		}
	}
	if len(fake.writes) != 2 {
		t.Fatalf("writes = %+v, want two", fake.writes)
	}
	byURI := map[string]string{}
	for _, write := range fake.writes {
		movies, _ := write.body["movies"].([]any)
		if len(movies) != 1 {
			t.Fatalf("write %s carries %d movies, want 1", write.uri, len(movies))
		}
		byURI[write.uri] = movies[0].(map[string]any)["ids"].(map[string]any)["imdb"].(string)
	}
	if byURI["/sync/history?allow_rewatch=yes"] != "tt1375666" || byURI["/sync/history"] != "tt0816692" {
		t.Fatalf("writes by URI = %v", byURI)
	}
}
