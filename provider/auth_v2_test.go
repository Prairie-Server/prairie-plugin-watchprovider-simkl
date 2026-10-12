package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

const (
	testV2ClientID     = "cid2-51d0e8"
	testV2Token        = "simkl_at_2c41e0d9b7a85f3c6e1d4b20a9f7e3c5d18"
	testV2RefreshToken = "simkl_rt_8e5a1f2c9d4b7e0a3c6f1d8b5e2a9c4f70"
	testDeviceCode     = "device-6a91f0"
)

func v2ProviderConfig(clientID, v2ClientID string) *pluginv1.WatchSyncProviderConfig {
	return &pluginv1.WatchSyncProviderConfig{Values: map[string]string{
		configClientID:   clientID,
		configV2ClientID: v2ClientID,
	}}
}

// v2Request is one /oauth2 request as Simkl received it.
type v2Request struct {
	path          string
	contentType   string
	authorization string
	form          url.Values
}

// v2Simkl serves the AUTH V2 endpoints: device answers POST /oauth2/device
// and token answers POST /oauth2/token. Any other request fails the test.
func v2Simkl(t *testing.T, token func(w http.ResponseWriter, form url.Values)) (*Server, func() []v2Request) {
	t.Helper()
	var mu sync.Mutex
	var requests []v2Request
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		mu.Lock()
		requests = append(requests, v2Request{
			path:          r.Method + " " + r.URL.Path,
			contentType:   r.Header.Get("Content-Type"),
			authorization: r.Header.Get("Authorization"),
			form:          r.PostForm,
		})
		mu.Unlock()
		switch r.Method + " " + r.URL.Path {
		case "POST " + oauth2DevicePath:
			writeJSON(t, w, `{"device_code":"`+testDeviceCode+`","user_code":"BDWP-HQPK","verification_uri":"https://simkl.com/pin","verification_uri_complete":"https://simkl.com/pin?user_code=BDWP-HQPK","expires_in":900,"interval":5}`)
		case "POST " + oauth2TokenPath:
			if token == nil {
				t.Errorf("unexpected token request")
				return
			}
			token(w, r.PostForm)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	return server, func() []v2Request {
		mu.Lock()
		defer mu.Unlock()
		return append([]v2Request(nil), requests...)
	}
}

func startV2(t *testing.T, server *Server, config *pluginv1.WatchSyncProviderConfig) *pluginv1.WatchSyncDeviceAuthorizationServiceStartResponse {
	t.Helper()
	response, err := server.DeviceAuthorization().Start(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: config,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return response
}

func pollV2(t *testing.T, server *Server, config *pluginv1.WatchSyncProviderConfig, state []byte) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	t.Helper()
	response, err := server.DeviceAuthorization().Poll(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServicePollRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: config,
		ProviderState:  state,
	})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return response
}

func oauth2Error(t *testing.T, w http.ResponseWriter, status int, code string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(t, w, `{"error":"`+code+`","error_description":"Simkl says `+testDeviceCode+`"}`)
}

func TestStartDeviceAuthUsesTheAuthV2DeviceFlow(t *testing.T) {
	server, requests := v2Simkl(t, nil)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }

	response := startV2(t, server, providerConfig(testV2ClientID))
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	got := requests()
	if len(got) != 1 {
		t.Fatalf("requests = %v, want only the device request", got)
	}
	request := got[0]
	if request.contentType != "application/x-www-form-urlencoded" || request.authorization != "" ||
		request.form.Get("client_id") != testV2ClientID || request.form.Get("scope") != "media:read media:write" {
		t.Fatalf("device request = %+v", request)
	}
	if response.GetUserCode() != "BDWP-HQPK" || response.GetVerificationUrl() != "https://simkl.com/pin" ||
		response.GetVerificationUrlComplete() != "https://simkl.com/pin?user_code=BDWP-HQPK" ||
		response.GetPollingInterval().AsDuration() != 5*time.Second ||
		!response.GetExpiresAt().AsTime().Equal(now.Add(15*time.Minute)) {
		t.Fatalf("response = %v", response)
	}
	var state signInState
	if err := json.Unmarshal(response.GetProviderState(), &state); err != nil || state.DeviceCode != testDeviceCode {
		t.Fatalf("provider state = %s, want the device code", response.GetProviderState())
	}
}

func TestStartDeviceAuthWithASeparateAuthV2AppSignsInThroughIt(t *testing.T) {
	server, requests := v2Simkl(t, nil)
	response := startV2(t, server, v2ProviderConfig(testClientID, testV2ClientID))
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if got := requests(); len(got) != 1 || got[0].form.Get("client_id") != testV2ClientID {
		t.Fatalf("requests = %+v, want one device request for the AUTH V2 app", got)
	}
}

func TestStartDeviceAuthDoesNotFallBackToPinCodesForTheAuthV2App(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !answerV1AppProbe(t, w, r) {
			t.Errorf("unexpected request %s", r.URL.RequestURI())
		}
	}))
	response := startV2(t, server, v2ProviderConfig(testClientID, testV2ClientID))
	fault := response.GetFault()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED {
		t.Fatalf("fault = %v, want permission denied", fault)
	}
	assertSafe(t, fault, testV2ClientID)
}

func TestStartDeviceAuthRejectsAnIncompleteDeviceCode(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"device_code":"`+testDeviceCode+`","user_code":"BDWP-HQPK","verification_uri":"","expires_in":900,"interval":5}`)
	}))
	response := startV2(t, server, providerConfig(testV2ClientID))
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
		t.Fatalf("fault = %v, want permanent", response.GetFault())
	}
}

func TestPollDeviceAuthReturnsAuthV2Credentials(t *testing.T) {
	server, requests := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
		writeJSON(t, w, `{"access_token":"`+testV2Token+`","token_type":"Bearer","expires_in":604800,"refresh_token":"`+testV2RefreshToken+`","scope":"media:read media:write"}`)
	})
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }
	config := v2ProviderConfig(testClientID, testV2ClientID)

	response := pollV2(t, server, config, startV2(t, server, config).GetProviderState())
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED {
		t.Fatalf("response = %v, want authorized", response)
	}
	token := requests()[1]
	if token.path != "POST "+oauth2TokenPath || token.form.Get("grant_type") != deviceCodeGrant ||
		token.form.Get("client_id") != testV2ClientID || token.form.Get("device_code") != testDeviceCode {
		t.Fatalf("token request = %+v", token)
	}
	credentials := response.GetCredentials()
	if credentials.GetAccessToken() != testV2Token || credentials.GetRefreshToken() != testV2RefreshToken ||
		credentials.GetTokenType() != "Bearer" || len(credentials.GetScopes()) != 2 ||
		!credentials.GetExpiresAt().AsTime().Equal(now.Add(7*24*time.Hour)) ||
		credentials.GetSecretAttributes()[clientIDAttribute] != testV2ClientID {
		t.Fatalf("credentials = %v", credentials)
	}
}

func TestPollDeviceAuthRejectsAReadOnlyGrant(t *testing.T) {
	server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
		writeJSON(t, w, `{"access_token":"`+testV2Token+`","token_type":"Bearer","expires_in":604800,"refresh_token":"`+testV2RefreshToken+`","scope":"media:read"}`)
	})
	config := providerConfig(testV2ClientID)
	response := pollV2(t, server, config, startV2(t, server, config).GetProviderState())
	fault := response.GetFault()
	if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED || response.GetCredentials() != nil {
		t.Fatalf("response = %v, want permission denied without credentials", response)
	}
	assertSafe(t, fault, testV2Token, testV2RefreshToken)
}

func TestPollDeviceAuthMapsAuthV2PollAnswers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		code       string
		wantStatus pluginv1.WatchSyncDeviceAuthorizationStatus
		wantFault  pluginv1.WatchSyncFaultCode
	}{
		{name: "pending", status: http.StatusBadRequest, code: "authorization_pending",
			wantStatus: pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING},
		{name: "expired", status: http.StatusBadRequest, code: "expired_token",
			wantStatus: pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED},
		{name: "invalid grant", status: http.StatusBadRequest, code: "invalid_grant",
			wantStatus: pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED},
		{name: "server app needs a secret", status: http.StatusUnauthorized, code: "invalid_client",
			wantFault: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED},
		{name: "unknown error", status: http.StatusBadRequest, code: "invalid_request",
			wantFault: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST},
		{name: "outage", status: http.StatusBadGateway, code: "server_error",
			wantFault: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
				oauth2Error(t, w, tc.status, tc.code)
			})
			config := providerConfig(testV2ClientID)
			response := pollV2(t, server, config, startV2(t, server, config).GetProviderState())
			if response.GetStatus() != tc.wantStatus || response.GetFault().GetCode() != tc.wantFault {
				t.Fatalf("response = %v, want status %v fault %v", response, tc.wantStatus, tc.wantFault)
			}
			if response.GetFault() != nil {
				assertSafe(t, response.GetFault(), testV2ClientID, testDeviceCode)
			}
		})
	}
}

func TestPollDeviceAuthSlowsDownEachTimeSimklAsks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{name: "slow_down", status: http.StatusBadRequest, code: "slow_down"},
		{name: "rate limited", status: http.StatusTooManyRequests, code: "rate_limit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
				oauth2Error(t, w, tc.status, tc.code)
			})
			config := providerConfig(testV2ClientID)
			state := startV2(t, server, config).GetProviderState()
			for _, want := range []time.Duration{10 * time.Second, 15 * time.Second} {
				response := pollV2(t, server, config, state)
				if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING ||
					response.GetPollingInterval().AsDuration() != want || response.ProviderState == nil {
					t.Fatalf("response = %v, want pending with a %s interval and new state", response, want)
				}
				state = response.GetProviderState()
			}
		})
	}
}

func TestPollDeviceAuthExpiresAnAuthV2CodeWithoutCallingSimkl(t *testing.T) {
	server, requests := v2Simkl(t, nil)
	config := providerConfig(testV2ClientID)
	state := startV2(t, server, config).GetProviderState()
	server.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	response := pollV2(t, server, config, state)
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED || len(requests()) != 1 {
		t.Fatalf("status = %v after %d requests, want expired without polling", response.GetStatus(), len(requests()))
	}
}

func TestPollDeviceAuthFinishesAPinSignInStartedBeforeTheUpgrade(t *testing.T) {
	// The provider_state earlier plugin versions stored for a PIN sign-in.
	state := []byte(`{"user_code":"ABCDE","expires_at":"` + time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339) + `"}`)
	server := pinServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"result":"OK","access_token":"`+testToken+`"}`)
	})
	response := pollOnce(t, server, state)
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED ||
		response.GetCredentials().GetAccessToken() != testToken {
		t.Fatalf("response = %v, want the PIN token", response)
	}
}

func TestEachTokenIsSentWithTheAppThatIssuedIt(t *testing.T) {
	issuedBy := func(clientID string) map[string]string { return map[string]string{clientIDAttribute: clientID} }
	for _, tc := range []struct {
		name       string
		config     *pluginv1.WatchSyncProviderConfig
		token      string
		attributes map[string]string
		want       string
	}{
		{name: "AUTH V1 token beside an AUTH V2 app", config: v2ProviderConfig(testClientID, testV2ClientID), token: testToken, want: testClientID},
		{name: "AUTH V2 token from the separate AUTH V2 app", config: v2ProviderConfig(testClientID, testV2ClientID), token: testV2Token, attributes: issuedBy(testV2ClientID), want: testV2ClientID},
		// An install that signed profiles in through an AUTH V2 app in the
		// Client ID setting, then entered a different AUTH V2 app.
		{name: "AUTH V2 token from the Client ID app after another AUTH V2 app was added", config: v2ProviderConfig(testClientID, testV2ClientID), token: testV2Token, attributes: issuedBy(testClientID), want: testClientID},
		{name: "AUTH V2 token from the only app", config: providerConfig(testV2ClientID), token: testV2Token, attributes: issuedBy(testV2ClientID), want: testV2ClientID},
		{name: "AUTH V1 token from the only app", config: providerConfig(testClientID), token: testToken, want: testClientID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotAPIKey, gotAuthorization string
			server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotAPIKey, gotAuthorization = r.Header.Get("simkl-api-key"), r.Header.Get("Authorization")
				writeJSON(t, w, `{"user":{"name":"quick"},"account":{"id":42}}`)
			}))
			response, _ := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: &pluginv1.WatchSyncAuthenticatedContext{
				CapabilityId:   capabilityID,
				ProviderConfig: tc.config,
				Credentials:    &pluginv1.WatchSyncCredentials{AccessToken: tc.token, SecretAttributes: tc.attributes},
			}})
			if response.GetFault() != nil || gotAPIKey != tc.want || gotAuthorization != "Bearer "+tc.token {
				t.Fatalf("fault = %v simkl-api-key = %q Authorization = %q, want %q", response.GetFault(), gotAPIKey, gotAuthorization, tc.want)
			}
		})
	}
}

func v2AuthContext(config *pluginv1.WatchSyncProviderConfig) *pluginv1.WatchSyncAuthenticatedContext {
	return &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId:   capabilityID,
		ProviderConfig: config,
		Credentials: &pluginv1.WatchSyncCredentials{
			AccessToken:      testV2Token,
			RefreshToken:     testV2RefreshToken,
			TokenType:        "Bearer",
			SecretAttributes: map[string]string{clientIDAttribute: testV2ClientID},
		},
	}
}

func TestRefreshCredentialsRenewsAnAuthV2Token(t *testing.T) {
	const renewed = "simkl_at_9f0e1d2c3b4a5968778695a4b3c2d1e0f12"
	for _, tc := range []struct {
		name          string
		refreshToken  string
		wantRefreshed string
	}{
		{name: "refresh token repeated", refreshToken: testV2RefreshToken, wantRefreshed: testV2RefreshToken},
		{name: "refresh token omitted", refreshToken: "", wantRefreshed: testV2RefreshToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, requests := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
				writeJSON(t, w, `{"access_token":"`+renewed+`","token_type":"Bearer","expires_in":604800,"refresh_token":"`+tc.refreshToken+`","scope":"media:read media:write"}`)
			})
			now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
			server.now = func() time.Time { return now }

			response, err := server.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{
				Context: v2AuthContext(v2ProviderConfig(testClientID, testV2ClientID)),
			})
			if err != nil || response.GetFault() != nil {
				t.Fatalf("RefreshCredentials: %v %v", err, response.GetFault())
			}
			request := requests()[0]
			if request.path != "POST "+oauth2TokenPath || request.form.Get("grant_type") != "refresh_token" ||
				request.form.Get("client_id") != testV2ClientID || request.form.Get("refresh_token") != testV2RefreshToken {
				t.Fatalf("refresh request = %+v", request)
			}
			credentials := response.GetCredentials()
			if credentials.GetAccessToken() != renewed || credentials.GetRefreshToken() != tc.wantRefreshed ||
				!credentials.GetExpiresAt().AsTime().Equal(now.Add(7*24*time.Hour)) ||
				credentials.GetSecretAttributes()[clientIDAttribute] != testV2ClientID {
				t.Fatalf("credentials = %v", credentials)
			}
		})
	}
}

func TestRefreshCredentialsMapsAuthV2Failures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
		want   pluginv1.WatchSyncFaultCode
	}{
		{name: "revoked or unused for 180 days", status: http.StatusBadRequest, code: "invalid_grant",
			want: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL},
		{name: "app removed", status: http.StatusUnauthorized, code: "invalid_client",
			want: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED},
		{name: "outage", status: http.StatusServiceUnavailable, code: "server_error",
			want: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_TEMPORARY},
		{name: "rate limited", status: http.StatusTooManyRequests, code: "rate_limit",
			want: pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_RATE_LIMITED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
				oauth2Error(t, w, tc.status, tc.code)
			})
			response, _ := server.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{
				Context: v2AuthContext(providerConfig(testV2ClientID)),
			})
			if response.GetFault().GetCode() != tc.want || response.GetCredentials() != nil {
				t.Fatalf("response = %v, want fault %v without credentials", response, tc.want)
			}
			assertSafe(t, response.GetFault(), testV2ClientID, testV2Token, testV2RefreshToken)
		})
	}
}

func TestStartDeviceAuthFallsBackToPinCodesOnA400InvalidClient(t *testing.T) {
	var pinRequested bool
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == oauth2DevicePath {
			oauth2Error(t, w, http.StatusBadRequest, "invalid_client")
			return
		}
		pinRequested = r.URL.Path == "/oauth/pin"
		writeJSON(t, w, `{"result":"OK","user_code":"ABCDE","verification_url":"https://simkl.com/pin","expires_in":900,"interval":5}`)
	}))
	response := startV2(t, server, providerConfig(testClientID))
	if response.GetFault() != nil || !pinRequested || response.GetUserCode() != "ABCDE" {
		t.Fatalf("response = %v pin requested = %v, want the PIN flow", response, pinRequested)
	}
}

func TestPollDeviceAuthAcceptsATokenResponseWithoutScope(t *testing.T) {
	server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
		writeJSON(t, w, `{"access_token":"`+testV2Token+`","token_type":"Bearer","expires_in":604800,"refresh_token":"`+testV2RefreshToken+`"}`)
	})
	config := providerConfig(testV2ClientID)
	response := pollV2(t, server, config, startV2(t, server, config).GetProviderState())
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED ||
		len(response.GetCredentials().GetScopes()) != 2 {
		t.Fatalf("response = %v, want authorized with the requested scopes", response)
	}
}

func TestPollDeviceAuthWaitsOutSimklsRetryAfter(t *testing.T) {
	server, _ := v2Simkl(t, func(w http.ResponseWriter, _ url.Values) {
		w.Header().Set("Retry-After", "30")
		oauth2Error(t, w, http.StatusTooManyRequests, "too_many_requests")
	})
	config := providerConfig(testV2ClientID)
	response := pollV2(t, server, config, startV2(t, server, config).GetProviderState())
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING ||
		response.GetPollingInterval().AsDuration() != 30*time.Second {
		t.Fatalf("response = %v, want pending with a 30s interval", response)
	}
}
