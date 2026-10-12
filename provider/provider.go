// Package provider implements Prairie's watch_sync_provider.v1 contract for
// Simkl. It is a port of the Simkl provider that Prairie used to build in, and
// keeps that provider's item keys, cursors, and upstream requests so existing
// connections carry over.
package provider

import (
	"context"
	"net/http"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	capabilityID     = "simkl"
	configClientID   = "app.client_id"
	configV2ClientID = "app.v2_client_id"

	// settingTrackRewatches is the connection setting that sends repeat plays
	// to Simkl as rewatches. It is off unless the profile turns it on.
	settingTrackRewatches = "track_rewatches"

	// The host cancels every RPC after two minutes. Upstream work stops this
	// long before that, so the plugin can still answer with a fault the host
	// understands instead of the call timing out.
	rpcDeadlineMargin = 5 * time.Second
)

// Server serves the WatchSyncProvider service. Register DeviceAuthorization
// alongside it: Simkl connects through device codes.
type Server struct {
	pluginv1.UnimplementedWatchSyncProviderServer
	simkl *simklClient
	now   func() time.Time
	// carryBudget bounds the rows a page token carries; see paging.go.
	carryBudget int
}

// NewServer returns a Simkl provider that sends requests with httpClient, or
// with a client using a 20-second request timeout when httpClient is nil.
func NewServer(httpClient *http.Client) *Server {
	return newServer(httpClient, defaultBaseURL)
}

func newServer(httpClient *http.Client, baseURL string) *Server {
	return &Server{simkl: newSimklClient(httpClient, baseURL), now: time.Now, carryBudget: carryBudgetBytes}
}

// DeviceAuthorization returns the WatchSyncDeviceAuthorizationService for
// this provider. It shares the server's Simkl client.
func (s *Server) DeviceAuthorization() pluginv1.WatchSyncDeviceAuthorizationServiceServer {
	return &deviceAuthServer{server: s}
}

// RefreshCredentials renews an AUTH V2 connection's access token, which lasts
// seven days. An AUTH V1 token does not expire and has no refresh token, so
// its credentials are returned unchanged.
func (s *Server) RefreshCredentials(ctx context.Context, req *pluginv1.WatchSyncRefreshCredentialsRequest) (*pluginv1.WatchSyncCredentialResponse, error) {
	acct, fault := s.authenticate(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncCredentialResponse{Fault: fault}, nil
	}
	refreshToken := strings.TrimSpace(req.GetContext().GetCredentials().GetRefreshToken())
	if refreshToken == "" {
		return &pluginv1.WatchSyncCredentialResponse{
			Credentials: cloneCredentials(req.GetContext().GetCredentials()),
		}, nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()
	credentials, fault := s.refreshV2(ctx, acct.clientID, refreshToken)
	return &pluginv1.WatchSyncCredentialResponse{Credentials: credentials, Fault: fault}, nil
}

func (s *Server) GetAccount(ctx context.Context, req *pluginv1.WatchSyncGetAccountRequest) (*pluginv1.WatchSyncGetAccountResponse, error) {
	acct, fault := s.authenticate(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncGetAccountResponse{Fault: fault}, nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()
	account, fault := s.lookupAccount(ctx, acct)
	return &pluginv1.WatchSyncGetAccountResponse{Account: account, Fault: fault}, nil
}

func (s *Server) ListRemoteState(ctx context.Context, req *pluginv1.WatchSyncListRemoteStateRequest) (*pluginv1.WatchSyncListRemoteStateResponse, error) {
	acct, fault := s.authenticate(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	kind, fault := requestedStateKind(req.GetStateKinds())
	if fault != nil {
		return &pluginv1.WatchSyncListRemoteStateResponse{Fault: fault}, nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()
	return s.listState(ctx, acct, kind, req), nil
}

// ApplyEvents applies each operation's events with one upstream write, the
// way the built-in provider sent each export batch. An upstream failure of
// that write fails the whole call, as the built-in returned an error for the
// whole batch; per-event outcomes cover titles Simkl could not match.
func (s *Server) ApplyEvents(ctx context.Context, req *pluginv1.WatchSyncApplyEventsRequest) (*pluginv1.WatchSyncApplyEventsResponse, error) {
	acct, fault := s.authenticate(req.GetContext())
	if fault != nil {
		return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()

	results := make(map[string]*pluginv1.WatchSyncApplyResult)
	var order []string
	var groups []operationGroup
	for _, event := range req.GetEvents() {
		id := event.GetEventId()
		if strings.TrimSpace(id) == "" {
			continue
		}
		if _, seen := results[id]; seen {
			// A repeated event ID is the same desired state, not a second
			// change, so it is applied once.
			continue
		}
		order = append(order, id)
		results[id] = nil
		if event.GetMedia() == nil {
			results[id] = rejected(id, invalidRequestFault("Watch event media is required"))
			continue
		}
		groups = addToGroup(groups, event)
	}
	for _, group := range groups {
		if fault := s.applyGroup(ctx, acct, group, results); fault != nil {
			return &pluginv1.WatchSyncApplyEventsResponse{Fault: fault}, nil
		}
	}
	response := &pluginv1.WatchSyncApplyEventsResponse{Results: make([]*pluginv1.WatchSyncApplyResult, 0, len(order))}
	for _, id := range order {
		result := results[id]
		if result == nil {
			result = retry(id, temporaryFault("Simkl event was not applied"))
		}
		response.Results = append(response.Results, result)
	}
	return response, nil
}

type operationGroup struct {
	operation pluginv1.WatchSyncOperation
	events    []*pluginv1.WatchSyncEvent
}

func addToGroup(groups []operationGroup, event *pluginv1.WatchSyncEvent) []operationGroup {
	for index := range groups {
		if groups[index].operation == event.GetOperation() {
			groups[index].events = append(groups[index].events, event)
			return groups
		}
	}
	return append(groups, operationGroup{operation: event.GetOperation(), events: []*pluginv1.WatchSyncEvent{event}})
}

func (s *Server) applyGroup(ctx context.Context, acct account, group operationGroup, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	switch group.operation {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_WATCHED:
		return s.markWatched(ctx, acct, group.events, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_UNWATCHED:
		return s.markUnwatched(ctx, acct, group.events, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_ADD_TO_WATCHLIST:
		return s.changeWatchlist(ctx, acct, group.events, "/sync/add-to-list", "plantowatch", results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_FROM_WATCHLIST:
		return s.changeWatchlist(ctx, acct, group.events, "/sync/remove-from-list", "", results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SET_RATING:
		return s.setRatings(ctx, acct, group.events, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_REMOVE_RATING:
		return s.removeRatings(ctx, acct, group.events, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_MARK_DROPPED:
		return s.changeDropped(ctx, acct, group.events, true, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_UNMARK_DROPPED:
		return s.changeDropped(ctx, acct, group.events, false, results)
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE,
		pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP:
		for _, event := range group.events {
			result, fault := s.scrobble(ctx, acct, event)
			if fault != nil {
				return fault
			}
			results[event.GetEventId()] = result
		}
		return nil
	default:
		for _, event := range group.events {
			results[event.GetEventId()] = rejected(event.GetEventId(), invalidRequestFault("Simkl does not support this watch operation"))
		}
		return nil
	}
}

// authenticate checks the capability, the admin's Simkl apps, and the
// profile's access token, and pairs the token with the app that issued it.
func (s *Server) authenticate(auth *pluginv1.WatchSyncAuthenticatedContext) (account, *pluginv1.WatchSyncFault) {
	apps, fault := configuredApps(auth.GetCapabilityId(), auth.GetProviderConfig())
	if fault != nil {
		return account{}, fault
	}
	token := strings.TrimSpace(auth.GetCredentials().GetAccessToken())
	if token == "" {
		return account{}, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			SafeMessage: "Simkl access token is missing; reconnect Simkl",
		}
	}
	return account{
		clientID:       apps.clientIDFor(auth.GetCredentials()),
		token:          token,
		trackRewatches: auth.GetConnectionSettings()[settingTrackRewatches] == "true",
	}, nil
}

// simklApps holds the client IDs of the admin's Simkl API apps. Simkl ties a
// token to the app that issued it, and an AUTH V1 app and an AUTH V2 app are
// separate registrations, so an install that connected profiles through an
// AUTH V1 app keeps that app as clientID and adds its AUTH V2 app as
// v2ClientID. Requests use the app recorded with the connection; see
// clientIDFor. Neither flow sends the app's client secret, so the plugin does
// not ask for it.
type simklApps struct {
	// clientID is app.client_id, an AUTH V1 or AUTH V2 app.
	clientID string
	// v2ClientID is app.v2_client_id, an optional AUTH V2 app that new
	// connections use instead of clientID.
	v2ClientID string
}

// signInClientID is the app new connections sign in through, and whether it
// is known to be an AUTH V2 app.
func (a simklApps) signInClientID() (string, bool) {
	if a.v2ClientID != "" {
		return a.v2ClientID, true
	}
	return a.clientID, false
}

// clientIDFor returns the app that issued credentials. An AUTH V2 connection
// records its app when it signs in. Credentials without that record come from
// the PIN flow, which only clientID can run.
func (a simklApps) clientIDFor(credentials *pluginv1.WatchSyncCredentials) string {
	if issuer := strings.TrimSpace(credentials.GetSecretAttributes()[clientIDAttribute]); issuer != "" {
		return issuer
	}
	return a.clientID
}

func configuredApps(capability string, config *pluginv1.WatchSyncProviderConfig) (simklApps, *pluginv1.WatchSyncFault) {
	if capability != capabilityID {
		return simklApps{}, invalidRequestFault("Unknown Simkl capability")
	}
	apps := simklApps{
		clientID:   configValue(config, configClientID),
		v2ClientID: configValue(config, configV2ClientID),
	}
	if apps.clientID == "" {
		return simklApps{}, permissionDeniedFault("Simkl is not set up: an administrator must enter the client ID of a Simkl API app in the plugin settings")
	}
	return apps, nil
}

func configValue(config *pluginv1.WatchSyncProviderConfig, key string) string {
	if value := strings.TrimSpace(config.GetValues()[key]); value != "" {
		return value
	}
	return strings.TrimSpace(config.GetSecretValues()[key])
}

func requestedStateKind(kinds []pluginv1.WatchSyncRemoteStateKind) (pluginv1.WatchSyncRemoteStateKind, *pluginv1.WatchSyncFault) {
	if len(kinds) == 0 {
		return pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED, nil
	}
	if len(kinds) != 1 {
		return 0, invalidRequestFault("Simkl reads one state family per traversal")
	}
	return kinds[0], nil
}

// withRPCDeadline ends upstream work rpcDeadlineMargin before the host's
// deadline for the call, unless less than that remains.
func withRPCDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) <= rpcDeadlineMargin {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, deadline.Add(-rpcDeadlineMargin))
}

func cloneCredentials(value *pluginv1.WatchSyncCredentials) *pluginv1.WatchSyncCredentials {
	if value == nil {
		return nil
	}
	clone := &pluginv1.WatchSyncCredentials{
		AccessToken:  value.GetAccessToken(),
		RefreshToken: value.GetRefreshToken(),
		ExpiresAt:    value.GetExpiresAt(),
		TokenType:    value.GetTokenType(),
		Scopes:       append([]string(nil), value.GetScopes()...),
	}
	if len(value.GetSecretAttributes()) > 0 {
		clone.SecretAttributes = make(map[string]string, len(value.GetSecretAttributes()))
		for key, attribute := range value.GetSecretAttributes() {
			clone.SecretAttributes[key] = attribute
		}
	}
	return clone
}

func applied(eventID string) *pluginv1.WatchSyncApplyResult {
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_APPLIED}
}

func noChange(eventID string) *pluginv1.WatchSyncApplyResult {
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_NO_CHANGE}
}

func rejected(eventID string, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_REJECTED, Fault: fault}
}

func retry(eventID string, fault *pluginv1.WatchSyncFault) *pluginv1.WatchSyncApplyResult {
	return &pluginv1.WatchSyncApplyResult{EventId: eventID, Status: pluginv1.WatchSyncApplyStatus_WATCH_SYNC_APPLY_STATUS_RETRY, Fault: fault}
}

// notFoundResult reports a title Simkl could not match.
func notFoundResult(eventID string) *pluginv1.WatchSyncApplyResult {
	return rejected(eventID, invalidRequestFault("Simkl could not find this title"))
}
