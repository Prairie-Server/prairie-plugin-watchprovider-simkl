package provider

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

func TestStartDeviceAuthFallsBackToPinCodesForAnAuthV1App(t *testing.T) {
	var gotPath, gotAPIKey, gotAuthorization string
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerV1AppProbe(t, w, r) {
			return
		}
		gotPath = r.URL.RequestURI()
		gotAPIKey = r.Header.Get("simkl-api-key")
		gotAuthorization = r.Header.Get("Authorization")
		writeJSON(t, w, map[string]any{
			"result":           "OK",
			"device_code":      "device-code",
			"user_code":        "ABCDE",
			"verification_url": "https://simkl.com/pin/",
			"expires_in":       900,
			"interval":         5,
		})
	}))
	now := time.Date(2026, 5, 4, 12, 0, 0, 0, time.UTC)
	server.now = func() time.Time { return now }

	response, err := server.DeviceAuthorization().Start(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: providerConfig(testClientID),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if response.GetFault() != nil {
		t.Fatalf("fault = %v", response.GetFault())
	}
	if gotPath != "/oauth/pin?client_id="+testClientID {
		t.Fatalf("path = %q, want pin path", gotPath)
	}
	if gotAPIKey != testClientID || gotAuthorization != "" {
		t.Fatalf("simkl-api-key = %q Authorization = %q", gotAPIKey, gotAuthorization)
	}
	if response.GetUserCode() != "ABCDE" || response.GetVerificationUrl() != "https://simkl.com/pin/" ||
		response.GetPollingInterval().AsDuration() != 5*time.Second {
		t.Fatalf("response = %v", response)
	}
	if !response.GetExpiresAt().AsTime().Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("expires_at = %s, want 15 minutes from now", response.GetExpiresAt().AsTime())
	}
	if len(response.GetProviderState()) == 0 {
		t.Fatal("provider state is empty")
	}
}

func TestStartDeviceAuthRejectsIncompleteResponse(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerV1AppProbe(t, w, r) {
			return
		}
		writeJSON(t, w, `{"result":"OK","user_code":"ABCDE","verification_url":"https://simkl.com/pin/","expires_in":0,"interval":5}`)
	}))
	response, _ := server.DeviceAuthorization().Start(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: providerConfig(testClientID),
	})
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMANENT {
		t.Fatalf("fault = %v, want permanent", response.GetFault())
	}
}

// answerV1AppProbe answers the AUTH V2 device request as Simkl answers it for
// an AUTH V1 app, and reports whether r was that request.
func answerV1AppProbe(t *testing.T, w http.ResponseWriter, r *http.Request) bool {
	t.Helper()
	if r.URL.Path != oauth2DevicePath {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	writeJSON(t, w, `{"error":"invalid_client","error_description":"This client_id is not enabled for OAuth 2.0"}`)
	return true
}

func startedPinState(t *testing.T, server *Server) []byte {
	t.Helper()
	response, err := server.DeviceAuthorization().Start(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: providerConfig(testClientID),
	})
	if err != nil || response.GetFault() != nil {
		t.Fatalf("Start: %v %v", err, response.GetFault())
	}
	return response.GetProviderState()
}

func pinServer(t *testing.T, poll func(w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if answerV1AppProbe(t, w, r) {
			return
		}
		if r.URL.Path == "/oauth/pin" {
			writeJSON(t, w, `{"result":"OK","device_code":"DEVICE","user_code":"ABCDE","verification_url":"https://simkl.com/pin","expires_in":900,"interval":5}`)
			return
		}
		if r.URL.Path != "/oauth/pin/ABCDE" || r.URL.Query().Get("client_id") != testClientID {
			t.Errorf("poll request = %s", r.URL.RequestURI())
		}
		poll(w, r)
	}))
	return server
}

func pollOnce(t *testing.T, server *Server, state []byte) *pluginv1.WatchSyncDeviceAuthorizationServicePollResponse {
	t.Helper()
	response, err := server.DeviceAuthorization().Poll(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServicePollRequest{
		CapabilityId:   capabilityID,
		ProviderConfig: providerConfig(testClientID),
		ProviderState:  state,
	})
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	return response
}

func TestPollDeviceAuthReportsPending(t *testing.T) {
	server := pinServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"result":"KO","message":"Authorization pending"}`)
	})
	response := pollOnce(t, server, startedPinState(t, server))
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_PENDING || response.GetFault() != nil {
		t.Fatalf("response = %v, want pending without a fault", response)
	}
}

func TestPollDeviceAuthReturnsTheTokenAsCompleteCredentials(t *testing.T) {
	server := pinServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{"result":"OK","access_token":" `+testToken+` "}`)
	})
	response := pollOnce(t, server, startedPinState(t, server))
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_AUTHORIZED {
		t.Fatalf("status = %v", response.GetStatus())
	}
	credentials := response.GetCredentials()
	if credentials.GetAccessToken() != testToken || credentials.GetRefreshToken() != "" || credentials.GetExpiresAt() != nil {
		t.Fatalf("credentials = %v, want the access token only", credentials)
	}
}

func TestPollDeviceAuthExpires(t *testing.T) {
	var polls atomic.Int32
	server := pinServer(t, func(w http.ResponseWriter, _ *http.Request) {
		polls.Add(1)
		writeJSON(t, w, `{"result":"KO"}`)
	})
	state := startedPinState(t, server)
	server.now = func() time.Time { return time.Now().Add(16 * time.Minute) }
	response := pollOnce(t, server, state)
	if response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED || polls.Load() != 0 {
		t.Fatalf("status = %v after %d polls, want expired without polling", response.GetStatus(), polls.Load())
	}

	unknown := pinServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	if response := pollOnce(t, unknown, startedPinState(t, unknown)); response.GetStatus() != pluginv1.WatchSyncDeviceAuthorizationStatus_WATCH_SYNC_DEVICE_AUTHORIZATION_STATUS_EXPIRED {
		t.Fatalf("status = %v for a code Simkl no longer knows, want expired", response.GetStatus())
	}
}

func TestPollDeviceAuthRejectsInvalidState(t *testing.T) {
	server := pinServer(t, func(http.ResponseWriter, *http.Request) {
		t.Error("an invalid state must not reach Simkl")
	})
	response := pollOnce(t, server, []byte("not json"))
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", response.GetFault())
	}
}

func TestMissingClientIDAsksAnAdministrator(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unconfigured plugin must not call Simkl")
	}))
	unconfigured := &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId:   capabilityID,
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{},
		Credentials:    &pluginv1.WatchSyncCredentials{AccessToken: testToken},
	}
	start, _ := server.DeviceAuthorization().Start(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServiceStartRequest{CapabilityId: capabilityID})
	poll, _ := server.DeviceAuthorization().Poll(context.Background(), &pluginv1.WatchSyncDeviceAuthorizationServicePollRequest{CapabilityId: capabilityID})
	account, _ := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: unconfigured})
	apply, _ := server.ApplyEvents(context.Background(), &pluginv1.WatchSyncApplyEventsRequest{Context: unconfigured})
	list, _ := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: unconfigured})
	for name, fault := range map[string]*pluginv1.WatchSyncFault{
		"start": start.GetFault(), "poll": poll.GetFault(), "account": account.GetFault(),
		"apply": apply.GetFault(), "list": list.GetFault(),
	} {
		if fault.GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_PERMISSION_DENIED ||
			fault.GetSafeMessage() == "" {
			t.Errorf("%s fault = %v, want permission denied", name, fault)
		}
	}
}

func TestSecretClientIDIsAccepted(t *testing.T) {
	var gotAPIKey string
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("simkl-api-key")
		writeJSON(t, w, `{"user":{"name":"quick"},"account":{"id":42}}`)
	}))
	response, _ := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: &pluginv1.WatchSyncAuthenticatedContext{
		CapabilityId:   capabilityID,
		ProviderConfig: &pluginv1.WatchSyncProviderConfig{SecretValues: map[string]string{configClientID: testClientID}},
		Credentials:    &pluginv1.WatchSyncCredentials{AccessToken: testToken},
	}})
	if response.GetFault() != nil || gotAPIKey != testClientID {
		t.Fatalf("fault = %v api key = %q", response.GetFault(), gotAPIKey)
	}
}

func TestGetAccountUsesTheAccountIDOrUserName(t *testing.T) {
	for _, tc := range []struct {
		name        string
		body        string
		wantSubject string
	}{
		{name: "account id", body: `{"user":{"name":"quick"},"account":{"id":42}}`, wantSubject: "42"},
		{name: "user name without id", body: `{"user":{"name":"quick"},"account":{}}`, wantSubject: "quick"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var method, path, authorization string
			server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				method, path, authorization = r.Method, r.URL.Path, r.Header.Get("Authorization")
				writeJSON(t, w, tc.body)
			}))
			response, err := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authContext()})
			if err != nil || response.GetFault() != nil {
				t.Fatalf("GetAccount: %v %v", err, response.GetFault())
			}
			if method != http.MethodPost || path != "/users/settings" || authorization != "Bearer "+testToken {
				t.Fatalf("request = %s %s %q", method, path, authorization)
			}
			account := response.GetAccount()
			if account.GetExternalSubject() != tc.wantSubject || account.GetUsername() != "quick" {
				t.Fatalf("account = %v", account)
			}
		})
	}
}

func TestGetAccountWithoutIdentityIsAFault(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(t, w, `{}`)
	}))
	response, _ := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authContext()})
	if response.GetFault() == nil || response.GetAccount() != nil {
		t.Fatalf("response = %v, want a fault", response)
	}
}

func TestRefreshCredentialsReturnsTheStoredTokenWithoutCallingSimkl(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("refresh must not call Simkl")
	}))
	auth := authContext()
	auth.Credentials.TokenType = "Bearer"
	response, err := server.RefreshCredentials(context.Background(), &pluginv1.WatchSyncRefreshCredentialsRequest{Context: auth})
	if err != nil || response.GetFault() != nil {
		t.Fatalf("RefreshCredentials: %v %v", err, response.GetFault())
	}
	if response.GetCredentials().GetAccessToken() != testToken || response.GetCredentials().GetTokenType() != "Bearer" {
		t.Fatalf("credentials = %v", response.GetCredentials())
	}
}

func TestMissingAccessTokenIsAnInvalidCredential(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("a request without a token must not call Simkl")
	}))
	response, _ := server.GetAccount(context.Background(), &pluginv1.WatchSyncGetAccountRequest{Context: authContextWithToken(" ")})
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_CREDENTIAL {
		t.Fatalf("fault = %v", response.GetFault())
	}
}

func TestUnknownCapabilityIsRejected(t *testing.T) {
	server, _ := newTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("an unknown capability must not call Simkl")
	}))
	auth := authContext()
	auth.CapabilityId = "trakt"
	response, _ := server.ListRemoteState(context.Background(), &pluginv1.WatchSyncListRemoteStateRequest{Context: auth})
	if response.GetFault().GetCode() != pluginv1.WatchSyncFaultCode_WATCH_SYNC_FAULT_CODE_INVALID_REQUEST {
		t.Fatalf("fault = %v", response.GetFault())
	}
}
