package main

import (
	"testing"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/manifest"
)

func TestManifestDeclaresTheSimklProvider(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	if parsed.GetPluginId() != "prairie.watchprovider.simkl" || parsed.GetVersion() != "0.4.0" {
		t.Fatalf("plugin = %q %q", parsed.GetPluginId(), parsed.GetVersion())
	}
	capabilities := parsed.GetCapabilities()
	if len(capabilities) != 1 {
		t.Fatalf("capabilities = %d, want 1", len(capabilities))
	}
	capability := capabilities[0]
	// The display name matches Prairie's former built-in Simkl provider.
	if capability.GetType() != "watch_sync_provider.v1" || capability.GetId() != "simkl" || capability.GetDisplayName() != "Simkl" {
		t.Fatalf("capability = %q %q %q", capability.GetType(), capability.GetId(), capability.GetDisplayName())
	}
	if len(capability.GetConfigSchema()) != 0 {
		t.Fatalf("connection config = %v, want none", capability.GetConfigSchema())
	}
}

func TestManifestAdvertisesTheBuiltInCapabilities(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	descriptor := parsed.GetCapabilities()[0].GetWatchSyncProvider()
	if len(descriptor.GetAuthMethods()) != 1 || descriptor.GetAuthMethods()[0] != pluginv1.WatchSyncAuthMethod_WATCH_SYNC_AUTH_METHOD_DEVICE_CODE {
		t.Fatalf("auth methods = %v, want device code", descriptor.GetAuthMethods())
	}
	if !descriptor.GetImportWatched() || !descriptor.GetImportProgress() || !descriptor.GetExportWatched() || !descriptor.GetExportUnwatched() ||
		!descriptor.GetImportWatchlist() || !descriptor.GetExportWatchlist() || !descriptor.GetRemoveWatchlist() ||
		!descriptor.GetScrobblePlayback() || !descriptor.GetImportRatings() || !descriptor.GetExportRatings() || !descriptor.GetSyncDropped() {
		t.Fatalf("descriptor = %v, want the built-in provider's capabilities", descriptor)
	}
	// Rating a movie on Simkl files it as watched, so the host must hold a
	// movie rating until the profile watched the movie, as it did for the
	// built-in provider. Series ratings are not held back.
	if gated := descriptor.GetRatingExportRequiresWatched(); len(gated) != 1 || gated[0] != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		t.Fatalf("rating_export_requires_watched = %v, want movies only", gated)
	}
	if descriptor.GetImportFavorites() || descriptor.GetExportFavorites() || descriptor.GetRemoveFavorites() || descriptor.GetProvidesWatchlistOrder() {
		t.Fatalf("descriptor = %v advertises favorites or watchlist order", descriptor)
	}
	media := map[pluginv1.WatchSyncMediaType]bool{}
	for _, mediaType := range descriptor.GetSupportedMediaTypes() {
		media[mediaType] = true
	}
	if len(media) != 3 || !media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE] ||
		!media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE] || !media[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES] {
		t.Fatalf("supported media types = %v", descriptor.GetSupportedMediaTypes())
	}
	if descriptor.GetMaxBatchSize() != 100 {
		t.Fatalf("max batch size = %d", descriptor.GetMaxBatchSize())
	}
}

func TestManifestAsksForPublicClientIDsOnly(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	schemas := parsed.GetGlobalConfigSchema()
	if len(schemas) != 1 || schemas[0].GetKey() != "app" || !schemas[0].GetRequired() {
		t.Fatalf("global config = %v", schemas)
	}
	// Simkl client IDs are public; neither sign-in flow sends a client secret.
	fields := schemas[0].GetAdminForm().GetFields()
	if len(fields) != 2 ||
		fields[0].GetKey() != "client_id" || fields[0].GetSecret() || !fields[0].GetRequired() ||
		fields[1].GetKey() != "v2_client_id" || fields[1].GetSecret() || fields[1].GetRequired() {
		t.Fatalf("fields = %v, want a required client_id and an optional v2_client_id, both public", fields)
	}
}

func TestManifestDeclaresTheRewatchSetting(t *testing.T) {
	t.Parallel()
	parsed, err := manifest.Load(manifestJSON)
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	// Rewatch tracking is off until the profile turns it on, as Simkl asks.
	settings := parsed.GetCapabilities()[0].GetWatchSyncProvider().GetConnectionSettings()
	if len(settings) != 1 || settings[0].GetKey() != "track_rewatches" || settings[0].GetLabel() != "Log rewatches" ||
		settings[0].GetType() != pluginv1.WatchSyncConnectionSettingType_WATCH_SYNC_CONNECTION_SETTING_TYPE_BOOLEAN ||
		settings[0].GetDefaultValue().GetBoolValue() {
		t.Fatalf("connection settings = %v, want an off-by-default track_rewatches switch", settings)
	}
}
