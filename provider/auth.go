package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	oauth2DevicePath = "/oauth2/device"
	oauth2TokenPath  = "/oauth2/token"

	deviceCodeGrant = "urn:ietf:params:oauth:grant-type:device_code"

	// The plugin writes watch history, lists, and ratings. AUTH V2 grants
	// read-only access unless media:write is asked for.
	v2Scope      = "media:read media:write"
	v2WriteScope = "media:write"

	// clientIDAttribute is the credential secret attribute that records which
	// app issued an AUTH V2 connection's tokens.
	clientIDAttribute = "client_id"

	// slowDownStep is how much a slow_down answer lengthens the polling
	// interval, as RFC 8628 prescribes.
	slowDownStep = 5 * time.Second
)

// deviceAuthServer connects a profile with a code the user enters at
// simkl.com/pin. An AUTH V2 app uses the RFC 8628 device flow; an AUTH V1 app
// uses Simkl's older PIN flow, which Simkl retires around April 2027.
type deviceAuthServer struct {
	pluginv1.UnimplementedWatchSyncDeviceAuthorizationServiceServer
	server *Server
}

// signInState is the provider_state the host keeps between polls. DeviceCode
// marks an AUTH V2 flow and UserCode an AUTH V1 one; a V1 state has the shape
// earlier plugin versions stored, so a sign-in in progress across an upgrade
// still completes.
type signInState struct {
	DeviceCode      string    `json:"device_code,omitempty"`
	IntervalSeconds int       `json:"interval_seconds,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	ExpiresAt       time.Time `json:"expires_at"`
}

// Start asks Simkl for a sign-in code. Without a separate AUTH V2 app, the
// configured app may be either version: Simkl answers an AUTH V1 client ID on
// the V2 endpoint with invalid_client, and the PIN flow is used instead.
func (d *deviceAuthServer) Start(ctx context.Context, req *pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest) (*pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse, error) {
	apps, fault := configuredApps(req.GetCapabilityId(), req.GetProviderConfig())
	if fault != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: fault}, nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()
	clientID, knownV2 := apps.signInClientID()
	response, isV1 := d.startV2(ctx, clientID)
	if isV1 && !knownV2 {
		return d.startV1(ctx, clientID), nil
	}
	if isV1 {
		response = &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{
			Fault: permissionDeniedFault("Simkl rejected the AUTH V2 client ID; check it in the plugin settings"),
		}
	}
	return response, nil
}

// startV2 starts the AUTH V2 device flow. isV1 reports that Simkl does not
// know clientID as an AUTH V2 app, which is how it answers an AUTH V1 app.
func (d *deviceAuthServer) startV2(ctx context.Context, clientID string) (_ *pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse, isV1 bool) {
	var response oauth2DeviceResponse
	status, code, fault := d.server.simkl.postOAuth2(ctx, oauth2DevicePath, url.Values{
		"client_id": {clientID},
		"scope":     {v2Scope},
	}, &response)
	switch {
	case fault != nil:
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: fault}, false
	case status == http.StatusUnauthorized, code == "invalid_client":
		// invalid_client: an unknown client ID or an AUTH V1 app.
		return nil, true
	case status != http.StatusOK:
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: oauth2Fault(status)}, false
	case response.DeviceCode == "" || response.UserCode == "" || response.VerificationURI == "" ||
		response.ExpiresIn <= 0 || response.Interval <= 0:
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{
			Fault: permanentFault("Simkl returned an incomplete device code"),
		}, false
	}
	expiresAt := d.server.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second)
	state, err := json.Marshal(signInState{
		DeviceCode:      response.DeviceCode,
		IntervalSeconds: response.Interval,
		ExpiresAt:       expiresAt,
	})
	if err != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: permanentFault("Simkl sign-in state could not be encoded")}, false
	}
	return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{
		UserCode:                response.UserCode,
		VerificationUrl:         response.VerificationURI,
		VerificationUrlComplete: response.VerificationURIComplete,
		ProviderState:           state,
		PollingInterval:         durationpb.New(time.Duration(response.Interval) * time.Second),
		ExpiresAt:               timestamppb.New(expiresAt),
	}, false
}

// startV1 starts the AUTH V1 PIN flow: GET /oauth/pin issues the code, and
// GET /oauth/pin/{code} answers with the access token once the user approves.
func (d *deviceAuthServer) startV1(ctx context.Context, clientID string) *pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse {
	var response pinCodeResponse
	if fault := d.server.simkl.get(ctx, account{clientID: clientID}, "/oauth/pin?client_id="+url.QueryEscape(clientID), &response); fault != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: fault}
	}
	if response.Result != "OK" || response.UserCode == "" || response.VerificationURL == "" ||
		response.ExpiresIn <= 0 || response.Interval <= 0 {
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{
			Fault: permanentFault("Simkl returned an incomplete PIN code"),
		}
	}
	// Simkl keeps device_code only for compatibility; polling uses the user
	// code, as the built-in provider did.
	expiresAt := d.server.now().UTC().Add(time.Duration(response.ExpiresIn) * time.Second)
	state, err := json.Marshal(signInState{UserCode: response.UserCode, ExpiresAt: expiresAt})
	if err != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{Fault: permanentFault("Simkl PIN state could not be encoded")}
	}
	return &pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse{
		UserCode:        response.UserCode,
		VerificationUrl: response.VerificationURL,
		ProviderState:   state,
		PollingInterval: durationpb.New(time.Duration(response.Interval) * time.Second),
		ExpiresAt:       timestamppb.New(expiresAt),
	}
}

func (d *deviceAuthServer) Poll(ctx context.Context, req *pluginv1.WatchSyncDeviceAuthorizationServicePollRequest) (*pluginv1.WatchSyncDeviceAuthorizationServicePollResponse, error) {
	apps, fault := configuredApps(req.GetCapabilityId(), req.GetProviderConfig())
	if fault != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Fault: fault}, nil
	}
	var state signInState
	if err := json.Unmarshal(req.GetProviderState(), &state); err != nil ||
		(strings.TrimSpace(state.DeviceCode) == "" && strings.TrimSpace(state.UserCode) == "") {
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
			Fault: invalidRequestFault("Simkl sign-in state is invalid; start connecting again"),
		}, nil
	}
	if !state.ExpiresAt.IsZero() && !d.server.now().Before(state.ExpiresAt) {
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED), nil
	}
	ctx, cancel := withRPCDeadline(ctx)
	defer cancel()
	if state.DeviceCode != "" {
		clientID, _ := apps.signInClientID()
		return d.pollV2(ctx, clientID, state), nil
	}
	return d.pollV1(ctx, apps.clientID, state), nil
}

// pollV2 exchanges the device code for tokens once the user approves. Simkl
// never reports a denial: a declined code stays pending until it expires.
func (d *deviceAuthServer) pollV2(ctx context.Context, clientID string, state signInState) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	var token oauth2TokenResponse
	status, code, fault := d.server.simkl.postOAuth2(ctx, oauth2TokenPath, url.Values{
		"grant_type":  {deviceCodeGrant},
		"client_id":   {clientID},
		"device_code": {state.DeviceCode},
	}, &token)
	switch {
	case status == http.StatusTooManyRequests:
		return slowDown(state, fault.GetRetryAfter().AsDuration())
	case fault != nil:
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Fault: fault}
	case status == http.StatusOK:
		// RFC 6749 section 5.1 lets the server omit scope when it grants
		// exactly the requested scope.
		if strings.TrimSpace(token.Scope) == "" {
			token.Scope = v2Scope
		}
		if !slices.Contains(strings.Fields(token.Scope), v2WriteScope) {
			return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
				Fault: permissionDeniedFault("Simkl granted read-only access, and Prairie needs to write watch history; connect Simkl again"),
			}
		}
		credentials := d.server.v2Credentials(token, clientID, "")
		if credentials == nil {
			return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Fault: temporaryFault("Simkl returned no access token")}
		}
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
			Status:      pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED,
			Credentials: credentials,
		}
	case code == "authorization_pending":
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING)
	case code == "slow_down":
		return slowDown(state, 0)
	case code == "expired_token", code == "invalid_grant":
		// invalid_grant covers a code issued to another client or already
		// used; it cannot complete either, so the user starts over.
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED)
	case code == "invalid_client":
		// The device endpoint accepted this client ID, so the token endpoint
		// wants a client secret: the app is a "Server apps & services" one.
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
			Fault: permissionDeniedFault(`Simkl requires a client secret for this app; register a Simkl app of the "TV, devices & command line" type and enter its client ID in the plugin settings`),
		}
	default:
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Fault: oauth2Fault(status)}
	}
}

// slowDown keeps the flow pending with an interval five seconds longer, or
// minimum when that is longer, such as a rate limit's Retry-After. The
// interval is stored in the flow state so the next slow_down lengthens it
// again.
func slowDown(state signInState, minimum time.Duration) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	interval := max(time.Duration(max(state.IntervalSeconds, 1))*time.Second+slowDownStep, minimum)
	state.IntervalSeconds = int((interval + time.Second - 1) / time.Second)
	interval = time.Duration(state.IntervalSeconds) * time.Second
	encoded, err := json.Marshal(state)
	if err != nil {
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING)
	}
	return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
		Status:          pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING,
		ProviderState:   encoded,
		PollingInterval: durationpb.New(interval),
	}
}

func (d *deviceAuthServer) pollV1(ctx context.Context, clientID string, state signInState) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	var response pinStatusResponse
	path := "/oauth/pin/" + url.PathEscape(state.UserCode) + "?client_id=" + url.QueryEscape(clientID)
	status, fault := d.server.simkl.do(ctx, account{clientID: clientID}, http.MethodGet, path, nil, &response)
	if status == http.StatusNotFound {
		// Simkl no longer knows the code, so this sign-in cannot complete.
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED)
	}
	if fault != nil {
		return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Fault: fault}
	}
	token := strings.TrimSpace(response.AccessToken)
	if response.Result != "OK" || token == "" {
		// Simkl answers a pending poll with 200 {"result":"KO"}.
		return pollStatus(pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING)
	}
	// A PIN token never expires and has no refresh token, so the access
	// token alone is the complete credential.
	return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{
		Status:      pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED,
		Credentials: &pluginv1.WatchSyncCredentials{AccessToken: token},
	}
}

func pollStatus(status pluginv1.WatchSyncDeviceAuthorizationStatus) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	return &pluginv1.WatchSyncDeviceAuthorizationServicePollResponse{Status: status}
}

// refreshV2 renews an AUTH V2 access token. Simkl's refresh tokens do not
// rotate, and each refresh extends the refresh token's 180 days.
func (s *Server) refreshV2(ctx context.Context, clientID, refreshToken string) (*pluginv1.WatchSyncCredentials, *pluginv1.WatchSyncFault) {
	var token oauth2TokenResponse
	status, code, fault := s.simkl.postOAuth2(ctx, oauth2TokenPath, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	}, &token)
	switch {
	case fault != nil:
		return nil, fault
	case status == http.StatusOK:
		credentials := s.v2Credentials(token, clientID, refreshToken)
		if credentials == nil {
			return nil, temporaryFault("Simkl returned no access token")
		}
		return credentials, nil
	case code == "invalid_grant":
		// The user revoked the connection on Simkl, or left it unused for
		// 180 days.
		return nil, &pluginv1.WatchSyncFault{
			Code:        pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL,
			SafeMessage: "Simkl no longer accepts this connection's sign-in; reconnect Simkl",
		}
	case code == "invalid_client":
		return nil, permissionDeniedFault("Simkl rejected the AUTH V2 client ID; check it in the plugin settings")
	default:
		return nil, oauth2Fault(status)
	}
}

// v2Credentials converts an AUTH V2 token response from clientID's app, or
// returns nil when it holds no access token. A response without a refresh
// token keeps the previous one. The credentials record clientID, because the
// tokens only work with the app that issued them.
func (s *Server) v2Credentials(token oauth2TokenResponse, clientID, previousRefresh string) *pluginv1.WatchSyncCredentials {
	accessToken := strings.TrimSpace(token.AccessToken)
	if accessToken == "" {
		return nil
	}
	credentials := &pluginv1.WatchSyncCredentials{
		AccessToken:      accessToken,
		RefreshToken:     strings.TrimSpace(token.RefreshToken),
		TokenType:        token.TokenType,
		Scopes:           strings.Fields(token.Scope),
		SecretAttributes: map[string]string{clientIDAttribute: clientID},
	}
	if credentials.RefreshToken == "" {
		credentials.RefreshToken = previousRefresh
	}
	if token.ExpiresIn > 0 {
		credentials.ExpiresAt = timestamppb.New(s.now().UTC().Add(time.Duration(token.ExpiresIn) * time.Second))
	}
	return credentials
}

// oauth2Fault describes an /oauth2 answer the caller has no specific meaning
// for. The error code is left out: it is Simkl's text, not the plugin's.
func oauth2Fault(status int) *pluginv1.WatchSyncFault {
	if status == http.StatusUnauthorized {
		return permissionDeniedFault("Simkl rejected the client ID; check it in the plugin settings")
	}
	return invalidRequestFault("Simkl rejected the sign-in request (HTTP " + strconv.Itoa(status) + ")")
}

// lookupAccount reads the Simkl account behind the token. The subject is the
// numeric account ID, or the user name when Simkl reports no ID, which is the
// identity the built-in provider recorded for each connection.
func (s *Server) lookupAccount(ctx context.Context, acct account) (*pluginv1.WatchSyncAccount, *pluginv1.WatchSyncFault) {
	var response userSettingsResponse
	if _, fault := s.simkl.post(ctx, acct, "/users/settings", nil, &response); fault != nil {
		return nil, fault
	}
	subject := strconv.Itoa(response.Account.ID)
	if response.Account.ID == 0 {
		subject = response.User.Name
	}
	if strings.TrimSpace(subject) == "" {
		return nil, permanentFault("Simkl did not return an account for this token")
	}
	return &pluginv1.WatchSyncAccount{
		ExternalSubject: subject,
		Username:        response.User.Name,
		DisplayName:     response.User.Name,
	}, nil
}
