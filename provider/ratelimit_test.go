package provider

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

var scrobbleStart = func() *pluginv1.WatchSyncEvent {
	return &pluginv1.WatchSyncEvent{
		EventId:         "scrobble:start:session",
		Operation:       pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		PositionSeconds: 60,
		DurationSeconds: 6000,
		Media: &pluginv1.WatchSyncMedia{
			MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
			ExternalIds: map[string]string{"imdb": "tt1375666"},
		},
	}
}

// respondingServer answers every request with status, Retry-After (when
// set), and body, counting attempts.
func respondingServer(t *testing.T, status int, retryAfter, body string, attempts *atomic.Int32) (*Server, *[]time.Duration) {
	t.Helper()
	return newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// emptyRead answers a read with an empty list of the right shape.
func emptyRead(t *testing.T, w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/sync/playback/") {
		writeJSON(t, w, `[]`)
		return
	}
	writeJSON(t, w, `{}`)
}

func listProgressFault(t *testing.T, server *Server) *pluginv1.WatchSyncFault {
	t.Helper()
	_, fault := listAllWithFault(t, server, kindProgress, "", 100)
	return fault
}

func requireRateLimited(t *testing.T, fault *pluginv1.WatchSyncFault) time.Duration {
	t.Helper()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED || fault.GetRetryAfter() == nil {
		t.Fatalf("fault = %v, want rate limited with retry_after", fault)
	}
	return fault.GetRetryAfter().AsDuration()
}

func TestDailyQuotaRateLimitUsesRetryAfterSeconds(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusTooManyRequests, "7200",
		`{"error":"user_limit_exceeded","code":429,"message":"This user has reached their daily API request limit"}`, &attempts)
	if retry := requireRateLimited(t, listProgressFault(t, server)); retry != 2*time.Hour {
		t.Fatalf("retry-after = %s, want 2h", retry)
	}
	if attempts.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("got %d attempts and waits %v; a daily quota must defer, not retry in place", attempts.Load(), *waits)
	}
}

func TestRateLimitUsesRetryAfterHTTPDate(t *testing.T) {
	var attempts atomic.Int32
	server, _ := respondingServer(t, http.StatusTooManyRequests, time.Now().Add(10*time.Minute).UTC().Format(http.TimeFormat), "", &attempts)
	// HTTP-dates have one-second resolution, so allow for truncation.
	if retry := requireRateLimited(t, listProgressFault(t, server)); retry <= 10*time.Minute-5*time.Second || retry > 10*time.Minute {
		t.Fatalf("retry-after = %s, want about 10m", retry)
	}
}

func TestRateLimitWithoutRetryAfterUsesFallback(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusTooManyRequests, "", "", &attempts)
	if retry := requireRateLimited(t, listProgressFault(t, server)); retry != defaultRetryAfter {
		t.Fatalf("retry-after = %s, want fallback %s", retry, defaultRetryAfter)
	}
	if attempts.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("got %d attempts and waits %v, want one attempt", attempts.Load(), *waits)
	}
}

func TestPerSecondRateLimitRetriesInPlaceIgnoringDailyRetryAfter(t *testing.T) {
	var attempts atomic.Int32
	server, waits := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			// Simkl documents that this Retry-After carries the daily reset.
			w.Header().Set("Retry-After", "50000")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate_limit","code":429}`))
			return
		}
		emptyRead(t, w, r)
	}))
	if fault := listProgressFault(t, server); fault != nil {
		t.Fatalf("fault after in-place retry: %v", fault)
	}
	// The activities read is sent twice, then both playback lists once.
	if attempts.Load() != 4 || len(*waits) != 1 || (*waits)[0] != perSecondRetryWait {
		t.Fatalf("got %d attempts and waits %v, want 4 attempts after one %s wait", attempts.Load(), *waits, perSecondRetryWait)
	}
}

func TestPerSecondRateLimitExhaustedDefersForFallback(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusTooManyRequests, "50000", `{"error":"rate_limit","code":429}`, &attempts)
	retry := requireRateLimited(t, listProgressFault(t, server))
	if attempts.Load() != maxRetryAttempts+1 || len(*waits) != maxRetryAttempts {
		t.Fatalf("got %d attempts and waits %v, want %d attempts", attempts.Load(), *waits, maxRetryAttempts+1)
	}
	if retry != defaultRetryAfter {
		t.Fatalf("retry-after = %s, want floored %s", retry, defaultRetryAfter)
	}
}

func TestWriteLockRetriesInPlaceWithSameBody(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	server, waits := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(raw))
		first := len(bodies) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"RATE_LIMIT","code":400}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	result := resultsByID(applyEvents(t, server, scrobbleStart()))["scrobble:start:session"]
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED {
		t.Fatalf("result after write lock cleared = %v", result)
	}
	if len(bodies) != 2 || bodies[0] == "" || bodies[0] != bodies[1] {
		t.Fatalf("body not replayed identically: %#v", bodies)
	}
	if len(*waits) != 1 || (*waits)[0] != writeLockRetryWait {
		t.Fatalf("got in-place waits %v, want [%s]", *waits, writeLockRetryWait)
	}
}

func TestPersistentWriteLockReturnsRateLimited(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusBadRequest, "", `{"error":"RATE_LIMIT","code":400}`, &attempts)
	response := applyEvents(t, server, scrobbleStart())
	retry := requireRateLimited(t, response.GetFault())
	if attempts.Load() != maxRetryAttempts+1 || len(*waits) != maxRetryAttempts {
		t.Fatalf("got %d attempts and waits %v, want %d attempts", attempts.Load(), *waits, maxRetryAttempts+1)
	}
	if retry != defaultRetryAfter {
		t.Fatalf("retry-after = %s, want %s", retry, defaultRetryAfter)
	}
}

func TestWriteLockWaitPastTheDeadlineDefersAtOnce(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusBadRequest, "", `{"error":"RATE_LIMIT","code":400}`, &attempts)
	// The plugin stops rpcDeadlineMargin early, which leaves less than the
	// five-second lock wait.
	ctx, cancel := context.WithTimeout(context.Background(), rpcDeadlineMargin+2*time.Second)
	defer cancel()
	response, err := server.ApplyEvents(ctx, &pluginv1.WatchSyncApplyEventsRequest{Context: authContext(), Events: []*pluginv1.WatchSyncEvent{scrobbleStart()}})
	if err != nil {
		t.Fatal(err)
	}
	if retry := requireRateLimited(t, response.GetFault()); retry != writeLockRetryWait {
		t.Fatalf("retry-after = %s, want %s", retry, writeLockRetryWait)
	}
	if attempts.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("got %d attempts and waits %v, want one attempt and no wait", attempts.Load(), *waits)
	}
}

func TestOtherBadRequestIsNotRateLimited(t *testing.T) {
	var attempts atomic.Int32
	server, waits := respondingServer(t, http.StatusBadRequest, "", `{"error":"wrong_parameter","code":400,"message":"bad `+testToken+`"}`, &attempts)
	fault := applyEvents(t, server, scrobbleStart()).GetFault()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v, want invalid request", fault)
	}
	assertSafe(t, fault)
	if attempts.Load() != 1 || len(*waits) != 0 {
		t.Fatalf("got %d attempts and waits %v, want no retry", attempts.Load(), *waits)
	}
}

func TestConflictIsNotRateLimited(t *testing.T) {
	var attempts atomic.Int32
	server, _ := respondingServer(t, http.StatusConflict, "", `{"error":"already_watched"}`, &attempts)
	event := scrobbleStart()
	event.Operation = pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE
	result := resultsByID(applyEvents(t, server, event))[event.GetEventId()]
	if result.GetStatus() != pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED || attempts.Load() != 1 {
		t.Fatalf("result = %v after %d attempts, want one rejected attempt", result, attempts.Load())
	}
}

func TestWriteLimiterPacesPerTokenAndLeavesReadsAlone(t *testing.T) {
	var writes, reads atomic.Int32
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			emptyRead(t, w, r)
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	// One write per hour: a second write for the same token can only proceed
	// by waiting, which the call's deadline refuses.
	server.simkl.writes = newCredentialLimiter(time.Hour, 1)
	start := func(ctx context.Context, token string) *pluginv1.WatchSyncApplyEventsResponse {
		response, err := server.ApplyEvents(ctx, &pluginv1.WatchSyncApplyEventsRequest{
			Context: authContextWithToken(token),
			Events:  []*pluginv1.WatchSyncEvent{scrobbleStart()},
		})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	if response := start(context.Background(), "token-a"); response.GetFault() != nil {
		t.Fatalf("first write for token-a: %v", response.GetFault())
	}
	deadline, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if retry := requireRateLimited(t, start(deadline, "token-a").GetFault()); retry != writeInterval {
		t.Fatalf("retry-after = %s, want %s", retry, writeInterval)
	}
	if response := start(context.Background(), "token-b"); response.GetFault() != nil {
		t.Fatalf("token-b must not wait behind token-a: %v", response.GetFault())
	}
	if writes.Load() != 2 {
		t.Fatalf("server saw %d writes, want 2", writes.Load())
	}
	for range 3 {
		if fault := listProgressFault(t, server); fault != nil {
			t.Fatalf("reads must not be paced: %v", fault)
		}
	}
	if reads.Load() != 3*3 {
		t.Fatalf("server saw %d reads, want 9", reads.Load())
	}
}

func TestCredentialLimiterHoldsNoRawToken(t *testing.T) {
	limiter := newCredentialLimiter(time.Second, 1)
	if err := limiter.Wait(context.Background(), testToken); err != nil {
		t.Fatal(err)
	}
	for key := range limiter.limiters {
		if string(key[:]) == testToken {
			t.Fatal("limiter keyed by the raw token")
		}
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		want  time.Duration
		ok    bool
	}{
		{"120", 2 * time.Minute, true},
		{"-1", 0, false},
		{"", 0, false},
		{"soon", 0, false},
		{now.Add(time.Minute).Format(http.TimeFormat), time.Minute, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	} {
		got, ok := parseRetryAfter(tc.value, now)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseRetryAfter(%q) = %s %v, want %s %v", tc.value, got, ok, tc.want, tc.ok)
		}
	}
}
