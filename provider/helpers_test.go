package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	testClientID = "cid-7b2e44"
	testToken    = "tok-3f9a1c"
)

// newTestServer serves handler as the Simkl API. The write limiter is
// relaxed and in-place retry waits are recorded instead of slept.
func newTestServer(t *testing.T, handler http.Handler) (*Server, *[]time.Duration) {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	server := newServer(upstream.Client(), upstream.URL)
	server.simkl.writes = newCredentialLimiter(time.Nanosecond, 100)
	var mu sync.Mutex
	waits := []time.Duration{}
	server.simkl.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		waits = append(waits, d)
		return nil
	}
	return server, &waits
}

func providerConfig(clientID string) *pluginv1.WatchSyncProviderConfig {
	return &pluginv1.WatchSyncProviderConfig{Values: map[string]string{configClientID: clientID}}
}

func authContext() *pluginv1.WatchSyncAuthenticatedContext {
	return authContextWithToken(testToken)
}

func authContextWithToken(token string) *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId:   capabilityID,
		ProviderConfig: providerConfig(testClientID),
		Credentials:    &pluginv1.WatchSyncCredentials{AccessToken: token},
	}
}

// writeJSON answers with body, which is either raw JSON text or a value to
// encode.
func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if text, ok := body.(string); ok {
		_, _ = w.Write([]byte(text))
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// traversal is every page of one ListRemoteState traversal, followed the way
// the host follows them.
type traversal struct {
	items    []*pluginv1.WatchSyncRemoteState
	cursor   string
	complete bool
	pages    int
	warnings []string
}

func listAll(t *testing.T, server *Server, kind pluginv1.WatchSyncRemoteStateKind, cursor string, pageSize int32) traversal {
	t.Helper()
	result, fault := listAllWithFault(t, server, kind, cursor, pageSize)
	if fault != nil {
		t.Fatalf("traversal fault = %v", fault)
	}
	return result
}

func listAllWithFault(t *testing.T, server *Server, kind pluginv1.WatchSyncRemoteStateKind, cursor string, pageSize int32) (traversal, *pluginv1.WatchSyncFault) {
	t.Helper()
	var result traversal
	seen := map[string]bool{}
	pageToken := ""
	for page := 0; page < 10_000; page++ {
		response, err := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{
			Context:    authContext(),
			Cursor:     cursor,
			PageToken:  pageToken,
			PageSize:   pageSize,
			StateKinds: []pluginv1.WatchSyncRemoteStateKind{kind},
		})
		if err != nil {
			t.Fatalf("ListRemoteState: %v", err)
		}
		if response.GetFault() != nil {
			return result, response.GetFault()
		}
		result.pages++
		if page > 0 && response.GetCompleteSnapshot() != result.complete {
			t.Fatalf("complete_snapshot changed on page %d", page+1)
		}
		result.complete = response.GetCompleteSnapshot()
		if pageSize > 0 && len(response.GetItems()) > int(pageSize) {
			t.Fatalf("page %d has %d items, more than page size %d", page+1, len(response.GetItems()), pageSize)
		}
		result.items = append(result.items, response.GetItems()...)
		for _, warning := range response.GetWarnings() {
			assertSafe(t, &pluginv1.WatchSyncFault{SafeMessage: warning})
		}
		result.warnings = append(result.warnings, response.GetWarnings()...)
		next := response.GetNextPageToken()
		if next == "" {
			result.cursor = response.GetNextCursor()
			return result, nil
		}
		if response.GetNextCursor() != "" {
			t.Fatalf("page %d returned a cursor before the final page", page+1)
		}
		if seen[next] {
			t.Fatalf("page %d repeated a page token", page+1)
		}
		seen[next] = true
		pageToken = next
	}
	t.Fatal("traversal did not end")
	return result, nil
}

// row is a remote state converted the way the host converts it into its
// RemoteWatch, RemoteProgress, RemoteFavorite, and RemoteRating rows, so the
// built-in provider's expectations apply unchanged.
type row struct {
	ProviderItemKey string
	Kind            string
	Title           string
	Year            int
	IMDbID          string
	TMDBID          string
	TVDBID          string
	SeriesTitle     string
	SeriesYear      int
	SeriesIMDbID    string
	SeriesTMDBID    string
	SeriesTVDBID    string
	SeasonNumber    int
	EpisodeNumber   int
	PlayCount       int
	LastWatchedAt   time.Time
	ProgressPercent float64
	PausedAt        time.Time
	Rating          int
	RatedAt         time.Time
}

func hostRow(state *pluginv1.WatchSyncRemoteState) row {
	media := state.GetMedia()
	converted := row{
		ProviderItemKey: state.GetProviderItemKey(),
		Title:           media.GetTitle(),
		Year:            int(media.GetYear()),
		IMDbID:          media.GetExternalIds()["imdb"],
		TMDBID:          media.GetExternalIds()["tmdb"],
		TVDBID:          media.GetExternalIds()["tvdb"],
		SeriesTitle:     media.GetSeriesTitle(),
		SeriesYear:      int(media.GetSeriesYear()),
		SeriesIMDbID:    media.GetSeriesExternalIds()["imdb"],
		SeriesTMDBID:    media.GetSeriesExternalIds()["tmdb"],
		SeriesTVDBID:    media.GetSeriesExternalIds()["tvdb"],
		SeasonNumber:    int(media.GetSeasonNumber()),
		EpisodeNumber:   int(media.GetEpisodeNumber()),
		PlayCount:       int(state.GetWatched().GetPlayCount()),
		ProgressPercent: state.GetProgress().GetProgressPercent(),
		Rating:          int(state.GetRating().GetRating()),
	}
	switch media.GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		converted.Kind = "movie"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		converted.Kind = "episode"
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES:
		converted.Kind = "series"
	}
	if value := state.GetWatched().GetLastWatchedAt(); value != nil {
		converted.LastWatchedAt = value.AsTime()
	}
	if value := state.GetProgress().GetPausedAt(); value != nil {
		converted.PausedAt = value.AsTime()
	}
	if value := state.GetRating().GetRatedAt(); value != nil {
		converted.RatedAt = value.AsTime()
	}
	return converted
}

func hostRows(states []*pluginv1.WatchSyncRemoteState) []row {
	rows := make([]row, 0, len(states))
	for _, state := range states {
		rows = append(rows, hostRow(state))
	}
	return rows
}

func rowsByKey(t *testing.T, states []*pluginv1.WatchSyncRemoteState) map[string]row {
	t.Helper()
	byKey := make(map[string]row, len(states))
	for _, state := range states {
		if _, duplicate := byKey[state.GetProviderItemKey()]; duplicate {
			t.Fatalf("duplicate provider item key %q", state.GetProviderItemKey())
		}
		byKey[state.GetProviderItemKey()] = hostRow(state)
	}
	return byKey
}

func applyEvents(t *testing.T, server *Server, events ...*pluginv1.WatchSyncEvent) *pluginv1.WatchSyncApplyEventsResponse {
	t.Helper()
	response, err := server.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{
		Context: authContext(),
		Events:  events,
	})
	if err != nil {
		t.Fatalf("ApplyEvents: %v", err)
	}
	return response
}

func resultsByID(response *pluginv1.WatchSyncApplyEventsResponse) map[string]*pluginv1.WatchSyncApplyResult {
	results := make(map[string]*pluginv1.WatchSyncApplyResult, len(response.GetResults()))
	for _, result := range response.GetResults() {
		results[result.GetEventId()] = result
	}
	return results
}

func movieEvent(id string, operation pluginv1.WatchSyncOperation, ids map[string]string) *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:   id,
		Operation: operation,
		Media: &pluginv1.WatchSyncMedia{
			MediaItemId: "media-" + id,
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: ids,
		},
	}
}

func seriesEvent(id string, operation pluginv1.WatchSyncOperation, ids map[string]string) *pluginv1.WatchSyncEvent {
	event := movieEvent(id, operation, ids)
	event.Media.MediaType = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
	return event
}

func timestamp(value time.Time) *timestamppb.Timestamp {
	return timestamppb.New(value)
}

// assertSafe fails when a fault message carries a credential.
func assertSafe(t *testing.T, fault *pluginv1.WatchSyncFault, secrets ...string) {
	t.Helper()
	for _, secret := range append(secrets, testToken, testClientID) {
		if secret != "" && strings.Contains(fault.GetSafeMessage(), secret) {
			t.Fatalf("fault message %q contains a secret", fault.GetSafeMessage())
		}
	}
}
