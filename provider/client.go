package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/types/known/durationpb"
)

const (
	defaultBaseURL        = "https://api.simkl.com"
	defaultRequestTimeout = 20 * time.Second
	// A large library's all-items read can run to tens of megabytes; the cap
	// only guards against an unbounded body.
	maxResponseBytes  = 64 << 20
	maxErrorBodyBytes = 4 << 10
)

// Simkl rate limits, from its rate-limits and errors docs: 10 GETs and 1 POST
// per second, plus a daily quota. Reads are sequential and few, so only POSTs
// are paced. Repeated POST overages can also earn the token or client_id a
// temporary throttling block (412 client_id_failed), which is not retried, so
// pacing is the main defense.
const (
	writeInterval = time.Second
	writeBurst    = 1

	// Simkl answers per-second overages with 429 {"error":"rate_limit"} and
	// says to retry in about a second. Its Retry-After on that response
	// carries the daily reset, so it is ignored.
	perSecondRetryWait = time.Second
	// A 400 {"error":"RATE_LIMIT"} is not a quota: it is a 20-second per-user
	// lock on /scrobble and the /sync writes. It clears as soon as the user's
	// in-flight write finishes, and Simkl says to retry shortly, without
	// exponential backoff.
	writeLockRetryWait = 5 * time.Second

	maxInPlaceRetryWait = 10 * time.Second
	maxRetryAttempts    = 2

	// Daily-quota 429s (user_limit_exceeded, app_limit_exceeded) carry
	// Retry-After, and per-second 429s clear in about a second, so a 429
	// without a usable Retry-After matches neither documented case. A minute
	// clears any per-second throttle or write lock with a wide margin without
	// parking the connection for long on a guess.
	defaultRetryAfter = time.Minute
)

// simklClient talks to the Simkl API. It is shared by every RPC: the write
// limiter must see all of a token's writes to pace them.
type simklClient struct {
	http    *http.Client
	baseURL string
	// writes paces POSTs per access token.
	writes *credentialLimiter
	// sleep waits between in-place rate-limit retries; tests replace it.
	sleep func(context.Context, time.Duration) error
	// maxResponse bounds a response body; tests lower it.
	maxResponse int64
}

func newSimklClient(httpClient *http.Client, baseURL string) *simklClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultRequestTimeout}
	}
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultBaseURL
	}
	return &simklClient{
		http:        httpClient,
		baseURL:     strings.TrimRight(baseURL, "/"),
		writes:      newCredentialLimiter(writeInterval, writeBurst),
		sleep:       sleepContext,
		maxResponse: maxResponseBytes,
	}
}

// account is what one RPC authenticates with: the install-wide client ID and,
// after sign-in, the profile's access token.
type account struct {
	clientID string
	token    string
	// trackRewatches is the profile's "Log rewatches" connection setting.
	trackRewatches bool
}

// get reads path into out.
func (c *simklClient) get(ctx context.Context, acct account, path string, out any) *pluginv1.WatchSyncFault {
	_, fault := c.do(ctx, acct, http.MethodGet, path, nil, out)
	return fault
}

// post writes payload to path and decodes the reply into out when out is set.
func (c *simklClient) post(ctx context.Context, acct account, path string, payload, out any) (int, *pluginv1.WatchSyncFault) {
	return c.do(ctx, acct, http.MethodPost, path, payload, out)
}

// do sends one request, retrying in place after a short rate-limit wait. It
// returns the final HTTP status, zero when no response arrived, and a fault
// for any outcome other than success.
func (c *simklClient) do(ctx context.Context, acct account, method, path string, payload, out any) (int, *pluginv1.WatchSyncFault) {
	var body []byte
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, permanentFault("Simkl request could not be encoded")
		}
		body = encoded
	}
	paced := acct.token != "" && method != http.MethodGet
	for attempt := 0; ; attempt++ {
		if paced {
			if err := c.writes.Wait(ctx, acct.token); err != nil {
				if ctx.Err() != nil {
					return 0, interruptedFault()
				}
				// The next write slot lies past the call's deadline. Nothing
				// was sent, so the host retries the work later.
				return 0, rateLimitedFault(writeInterval)
			}
		}
		status, wait, limited, fault := c.doOnce(ctx, acct, method, path, body, out)
		if !limited {
			return status, fault
		}
		if attempt < maxRetryAttempts && wait <= maxInPlaceRetryWait {
			if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= wait {
				return status, rateLimitedFault(wait)
			}
			if err := c.sleep(ctx, wait); err != nil {
				return status, interruptedFault()
			}
			continue
		}
		// Repeated short waits that still end rate limited are not
		// trustworthy, so back off for a full fallback window instead.
		if attempt >= maxRetryAttempts && wait < defaultRetryAfter {
			wait = defaultRetryAfter
		}
		return status, rateLimitedFault(wait)
	}
}

// doOnce performs a single HTTP attempt. A rate-limit response reports limited
// with how long to wait before retrying; every other outcome reports its
// fault, if any.
func (c *simklClient) doOnce(ctx context.Context, acct account, method, path string, body []byte, out any) (status int, wait time.Duration, limited bool, fault *pluginv1.WatchSyncFault) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, 0, false, permanentFault("Simkl request could not be created")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("simkl-api-key", acct.clientID)
	if acct.token != "" {
		req.Header.Set("Authorization", "Bearer "+acct.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return 0, 0, false, interruptedFault()
		}
		return 0, 0, false, temporaryFault("Simkl is temporarily unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		if out == nil || resp.StatusCode == http.StatusNoContent {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
			return resp.StatusCode, 0, false, nil
		}
		body := &io.LimitedReader{R: resp.Body, N: c.maxResponse + 1}
		if err := json.NewDecoder(body).Decode(out); err != nil {
			if body.N <= 0 {
				return resp.StatusCode, 0, false, permanentFault("Simkl returned a response larger than the plugin reads")
			}
			return resp.StatusCode, 0, false, temporaryFault("Simkl returned an unreadable response")
		}
		return resp.StatusCode, 0, false, nil
	}
	code := ""
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusBadRequest {
		code = errorCode(resp.Body)
	}
	if wait, limited := rateLimitWait(resp, code); limited {
		return resp.StatusCode, wait, true, nil
	}
	return resp.StatusCode, 0, false, faultForStatus(resp.StatusCode, code)
}

// postOAuth2 sends one form-encoded request to an AUTH V2 /oauth2 endpoint,
// which takes the client ID in the body rather than the simkl-api-key header.
// A 400 or 401 is returned with its RFC 6749 error code and no fault, because
// the device flow answers a pending sign-in with 400 and the caller knows what
// each code means. Every other failure is a fault.
func (c *simklClient) postOAuth2(ctx context.Context, path string, form url.Values, out any) (status int, code string, fault *pluginv1.WatchSyncFault) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", permanentFault("Simkl request could not be created")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, context.DeadlineExceeded) {
			return 0, "", interruptedFault()
		}
		return 0, "", temporaryFault("Simkl is temporarily unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices:
		if err := json.NewDecoder(io.LimitReader(resp.Body, c.maxResponse)).Decode(out); err != nil {
			return resp.StatusCode, "", temporaryFault("Simkl returned an unreadable sign-in response")
		}
		return resp.StatusCode, "", nil
	case resp.StatusCode == http.StatusBadRequest, resp.StatusCode == http.StatusUnauthorized:
		return resp.StatusCode, errorCode(resp.Body), nil
	case resp.StatusCode == http.StatusTooManyRequests:
		wait, _ := rateLimitWait(resp, errorCode(resp.Body))
		return resp.StatusCode, "", rateLimitedFault(wait)
	case resp.StatusCode >= http.StatusInternalServerError:
		return resp.StatusCode, "", temporaryFault(fmt.Sprintf("Simkl is temporarily unavailable (HTTP %d)", resp.StatusCode))
	default:
		return resp.StatusCode, "", permanentFault(fmt.Sprintf("Simkl sign-in request failed (HTTP %d)", resp.StatusCode))
	}
}

// rateLimitWait classifies Simkl's throttling responses, which share status
// codes with unrelated errors and are told apart by the body's error field.
func rateLimitWait(resp *http.Response, code string) (time.Duration, bool) {
	switch {
	case resp.StatusCode == http.StatusTooManyRequests && strings.EqualFold(code, "rate_limit"):
		return perSecondRetryWait, true
	case resp.StatusCode == http.StatusTooManyRequests:
		wait, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if !ok {
			wait = defaultRetryAfter
		}
		return wait, true
	case resp.StatusCode == http.StatusBadRequest && strings.EqualFold(code, "rate_limit"):
		return writeLockRetryWait, true
	default:
		return 0, false
	}
}

// faultForStatus maps a failed Simkl response. Messages carry the status code
// only: Simkl error bodies can echo request data.
func faultForStatus(status int, code string) *pluginv1.WatchSyncFault {
	switch {
	case status == http.StatusUnauthorized:
		return &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			SafeMessage: "Simkl rejected the access token; reconnect Simkl",
		}
	case status == http.StatusBadRequest && code == "unauthorized_client":
		return permissionDeniedFault("Simkl rejected the client ID: it belongs to a Simkl AUTH V2 app, which cannot use AUTH V1 sign-in")
	case status == http.StatusPreconditionFailed:
		return permissionDeniedFault("Simkl rejected the client ID; check the Simkl client ID in the plugin settings, or wait if Simkl blocked it for sending too many writes")
	case status == http.StatusForbidden:
		return permissionDeniedFault("Simkl refused the request (HTTP 403)")
	case status == http.StatusBadRequest, status == http.StatusNotFound, status == http.StatusConflict,
		status == http.StatusUnprocessableEntity:
		return invalidRequestFault(fmt.Sprintf("Simkl rejected the request (HTTP %d)", status))
	case status >= http.StatusInternalServerError:
		return temporaryFault(fmt.Sprintf("Simkl is temporarily unavailable (HTTP %d)", status))
	default:
		return permanentFault(fmt.Sprintf("Simkl request failed (HTTP %d)", status))
	}
}

// errorCode reads the machine-readable error field from a Simkl error body.
func errorCode(body io.Reader) string {
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(body, maxErrorBodyBytes)).Decode(&envelope); err != nil {
		return ""
	}
	return strings.TrimSpace(envelope.Error)
}

func appendDateFrom(path string, dateFrom string) string {
	if strings.TrimSpace(dateFrom) == "" {
		return path
	}
	separator := "?"
	if strings.Contains(path, "?") {
		separator = "&"
	}
	return path + separator + "date_from=" + url.QueryEscape(dateFrom)
}

func rateLimitedFault(retryAfter time.Duration) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED,
		SafeMessage: "Simkl rate limit reached",
		RetryAfter:  durationpb.New(retryAfter),
	}
}

func interruptedFault() *pluginv1.WatchSyncFault {
	return temporaryFault("Simkl did not answer within the sync time limit")
}

func temporaryFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY,
		SafeMessage: message,
	}
}

func permanentFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT,
		SafeMessage: message,
	}
}

func invalidRequestFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST,
		SafeMessage: message,
	}
}

func permissionDeniedFault(message string) *pluginv1.WatchSyncFault {
	return &pluginv1.WatchSyncFault{
		Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED,
		SafeMessage: message,
	}
}
