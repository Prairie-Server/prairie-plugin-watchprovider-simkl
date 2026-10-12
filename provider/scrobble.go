package provider

import (
	"context"
	"net/http"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// scrobble forwards a playback start, pause, or stop. Simkl answers a
// completed stop it already recorded, for example a redelivered one, with
// 409 already_watched, which means the stop is applied.
func (s *Server) scrobble(ctx context.Context, acct account, event *pluginv1.WatchSyncEvent) (*pluginv1.WatchSyncApplyResult, *pluginv1.WatchSyncFault) {
	var path string
	switch event.GetOperation() {
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_START:
		path = "/scrobble/start"
	case pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_PAUSE:
		path = "/scrobble/pause"
	default:
		path = "/scrobble/stop"
	}
	payload, fault := buildScrobblePayload(event)
	if fault != nil {
		return rejected(event.GetEventId(), fault), nil
	}
	status, fault := s.simkl.post(ctx, acct, path, payload, nil)
	if status == http.StatusConflict {
		if event.GetOperation() == pluginv1.WatchSyncOperation_WATCH_SYNC_OPERATION_SCROBBLE_STOP && event.GetCompleted() {
			return noChange(event.GetEventId()), nil
		}
		return rejected(event.GetEventId(), invalidRequestFault("Simkl refused the playback event as a conflict (HTTP 409)")), nil
	}
	if fault != nil {
		return nil, fault
	}
	return applied(event.GetEventId()), nil
}

func buildScrobblePayload(event *pluginv1.WatchSyncEvent) (map[string]any, *pluginv1.WatchSyncFault) {
	media := event.GetMedia()
	progress := 0.0
	if event.GetDurationSeconds() > 0 {
		progress = event.GetPositionSeconds() / event.GetDurationSeconds() * 100
	}
	payload := map[string]any{"progress": progress}
	ids := media.GetExternalIds()
	switch media.GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
		series := media.GetSeriesExternalIds()
		payload["show"] = map[string]any{"ids": idsFromLocal(series["imdb"], series["tmdb"], series["tvdb"])}
		payload["episode"] = map[string]any{
			"season": int(media.GetSeasonNumber()),
			"number": int(media.GetEpisodeNumber()),
			"ids":    idsFromLocal(ids["imdb"], ids["tmdb"], ids["tvdb"]),
		}
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
		payload["movie"] = map[string]any{"ids": idsFromLocal(ids["imdb"], ids["tmdb"], ids["tvdb"])}
	default:
		return nil, invalidRequestFault("Simkl scrobbles movies and episodes only")
	}
	return payload, nil
}
