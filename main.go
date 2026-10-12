package main

import (
	_ "embed"

	"github.com/prairie-server/prairie-plugin-sdk/pkg/pluginsdk/runtime"

	"github.com/prairie-server/prairie-plugin-watchprovider-simkl/provider"
)

// version is set at build time with -ldflags "-X main.version=...".
var version string

//go:embed manifest.json
var manifestJSON []byte

func main() {
	server := provider.NewServer(nil)
	runtime.ServeManifestWithOptions(manifestJSON, version, runtime.CapabilityServers{
		WatchSyncProvider: server,
	}, runtime.WithWatchSyncDeviceAuthorization(server.DeviceAuthorization()))
}
