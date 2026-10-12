package provider

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/binary"
	"math"
	"net/http"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

func TestLocalPlayKeyPrefersTheMostSpecificID(t *testing.T) {
	t.Parallel()
	episode := pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE
	movie := pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE
	cases := []struct {
		name string
		play localPlay
		want string
	}{
		{"episode tvdb", localPlay{kind: episode, tvdbID: "9", tmdbID: "8"}, "tvdb:9"},
		{"episode tmdb", localPlay{kind: episode, tmdbID: "8"}, "tmdb:8"},
		{"episode series tvdb", localPlay{kind: episode, seriesTVDBID: "7", seasonNumber: 2, episodeNumber: 3}, "show:tvdb:7:s2:e3"},
		{"episode series tmdb", localPlay{kind: episode, seriesTMDBID: "6", seasonNumber: 1, episodeNumber: 4}, "show:tmdb:6:s1:e4"},
		{"episode series imdb", localPlay{kind: episode, seriesIMDbID: "tt5", seasonNumber: 1, episodeNumber: 1}, "show:imdb:tt5:s1:e1"},
		{"movie imdb", localPlay{kind: movie, imdbID: "tt1", tmdbID: "2"}, "imdb:tt1"},
		{"movie tmdb", localPlay{kind: movie, tmdbID: "2", tvdbID: "3"}, "tmdb:2"},
		{"movie tvdb", localPlay{kind: movie, tvdbID: "3"}, "tvdb:3"},
		{"movie none", localPlay{kind: movie}, ""},
		{"episode none", localPlay{kind: episode}, ""},
	}
	for _, tc := range cases {
		if got := localPlayKey(tc.play); got != tc.want {
			t.Errorf("%s: key = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestIDsFromProviderItemKey(t *testing.T) {
	t.Parallel()
	cases := map[string]simklIDs{
		"imdb:tt1":  {IMDb: "tt1"},
		"tmdb:22":   {TMDB: 22},
		"tvdb:33":   {TVDB: 33},
		"simkl:44":  {Simkl: 44},
		"other:5":   {},
		"imdb:":     {},
		"no-prefix": {},
	}
	for key, want := range cases {
		if got := idsFromProviderItemKey(key); got != want {
			t.Errorf("idsFromProviderItemKey(%q) = %+v, want %+v", key, got, want)
		}
	}
}

func TestHistoryMatchKeyHelpers(t *testing.T) {
	t.Parallel()
	if got := historyIDMatchKeys(simklIDs{IMDb: "tt1", TMDB: 2, TVDB: 3, Simkl: 4}); len(got) != 4 || got[3] != "simkl:4" {
		t.Fatalf("keys = %v", got)
	}
	if got := appendHistoryWatchedVariants([]string{"a"}, "", "2026-01-01"); len(got) != 1 {
		t.Fatalf("empty base appended keys: %v", got)
	}
	got := appendHistoryWatchedVariants(nil, "imdb:tt1", "2026-01-01")
	if len(got) != 2 || got[0] != "imdb:tt1:watched:2026-01-01" || got[1] != "imdb:tt1" {
		t.Fatalf("variants = %v", got)
	}
	_ = historyShowMatchKeys(simklHistoryShow{})
	request := simklHistoryRequest{eventIDsByKey: map[string][]string{}}
	request.addEventID("", []string{"k"})
	request.addEventID("e1", []string{"", "k"})
	if len(request.eventIDsByKey) != 1 || request.eventIDsByKey["k"][0] != "e1" {
		t.Fatalf("eventIDsByKey = %v", request.eventIDsByKey)
	}
}

func TestOldestCursorSkipsBlankAndKeepsUnparseable(t *testing.T) {
	t.Parallel()
	if got := oldestCursor("", "  ", "2026-02-01T00:00:00Z", "2026-01-01T00:00:00Z"); got != "2026-01-01T00:00:00Z" {
		t.Fatalf("oldest = %q", got)
	}
	if got := oldestCursor("not-a-time", "also-bad"); got != "not-a-time" {
		t.Fatalf("oldest of unparseable = %q, want the first", got)
	}
}

func TestPageSizeAndHandedOut(t *testing.T) {
	t.Parallel()
	if pageSize(0) != defaultPageSize || pageSize(-3) != defaultPageSize {
		t.Fatal("non-positive page size must use the default")
	}
	if pageSize(math.MaxInt32) != maxPageSize {
		t.Fatal("page size must be capped")
	}
	token := &pageToken{}
	token.handedOut("a")
	token.handedOut("a")
	token.handedOut("b")
	if token.After != "b" || token.AfterCount != 1 {
		t.Fatalf("after = %q x%d", token.After, token.AfterCount)
	}
}

func gzipBytes(t *testing.T, parts ...[]byte) string {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	for _, part := range parts {
		if _, err := writer.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(buffer.Bytes())
}

// A page token the host hands back is untrusted: every way it can be
// corrupted is answered with an invalid-request fault, never a panic.
func TestDecodePageTokenRejectsCorruptTokens(t *testing.T) {
	t.Parallel()
	kind := pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED
	lengthPrefix := func(n uint64) []byte { return binary.AppendUvarint(nil, n) }
	cases := map[string]string{
		"not base64":        "!!!",
		"not gzip":          base64.RawURLEncoding.EncodeToString([]byte("plain")),
		"no length prefix":  gzipBytes(t, nil),
		"length past data":  gzipBytes(t, lengthPrefix(50), []byte("{}")),
		"header not json":   gzipBytes(t, lengthPrefix(3), []byte("abc")),
		"rows not protobuf": gzipBytes(t, lengthPrefix(2), []byte("{}"), []byte{0xff, 0xff, 0xff}),
	}
	for name, raw := range cases {
		token, fault := decodePageToken(raw, kind, "cursor")
		if token != nil || fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
			t.Errorf("%s: token=%v fault=%v, want an invalid-request fault", name, token, fault.GetCode())
		}
	}
}

func TestRequestedStateKind(t *testing.T) {
	t.Parallel()
	watched := pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED
	if kind, fault := requestedStateKind(nil); kind != watched || fault != nil {
		t.Fatalf("default kind = %v, %v", kind, fault)
	}
	if _, fault := requestedStateKind([]pluginv1.WatchSyncRemoteStateKind{watched, watched}); fault == nil {
		t.Fatal("two kinds must be refused")
	}
}

func TestCloneCredentialsCopiesSecretAttributes(t *testing.T) {
	t.Parallel()
	if cloneCredentials(nil) != nil {
		t.Fatal("nil credentials must clone to nil")
	}
	original := &pluginv1.WatchSyncCredentials{AccessToken: "a", SecretAttributes: map[string]string{"k": "v"}}
	clone := cloneCredentials(original)
	clone.SecretAttributes["k"] = "changed"
	if original.SecretAttributes["k"] != "v" || clone.AccessToken != "a" {
		t.Fatalf("clone shares state with the original: %v", original.SecretAttributes)
	}
}

func TestDefaultConstructionAndFaultHelpers(t *testing.T) {
	t.Parallel()
	if NewServer(nil) == nil {
		t.Fatal("NewServer returned nil")
	}
	client := newSimklClient(nil, "  ")
	if client.http == nil || client.baseURL != defaultBaseURL {
		t.Fatalf("defaults not applied: %v %q", client.http, client.baseURL)
	}
	if retry("e", temporaryFault("x")).GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY {
		t.Fatal("retry result must carry the retry status")
	}
	if oauth2Fault(http.StatusUnauthorized).GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED {
		t.Fatal("401 must map to permission denied")
	}
	if interruptedFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY {
		t.Fatal("an interrupted request is temporary")
	}
}

func TestParseRetryAfterSaturates(t *testing.T) {
	t.Parallel()
	got, ok := parseRetryAfter("99999999999999999", time.Now())
	if !ok || got != time.Duration(math.MaxInt64) {
		t.Fatalf("got %v, %v", got, ok)
	}
}

func TestSleepContext(t *testing.T) {
	t.Parallel()
	if err := sleepContext(context.Background(), 0); err != nil {
		t.Fatalf("zero sleep: %v", err)
	}
	if err := sleepContext(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("short sleep: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepContext(ctx, 0); err == nil {
		t.Fatal("cancelled zero sleep returned nil")
	}
	if err := sleepContext(ctx, time.Hour); err == nil {
		t.Fatal("cancelled sleep returned nil")
	}
}

func TestCredentialLimiterSweepsIdleEntries(t *testing.T) {
	t.Parallel()
	clock := time.Now()
	l := newCredentialLimiter(time.Millisecond, 0)
	l.now = func() time.Time { return clock }
	if err := l.Wait(context.Background(), "idle"); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(credentialLimiterIdleTTL + time.Minute)
	if err := l.Wait(context.Background(), "fresh"); err != nil {
		t.Fatal(err)
	}
	if got := len(l.limiters); got != 1 {
		t.Fatalf("limiters after sweep = %d, want only the fresh one", got)
	}
}
