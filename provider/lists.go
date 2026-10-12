package provider

import (
	"context"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// changeWatchlist adds titles to Simkl's "plan to watch" list (to set) or
// removes them from the user's lists (to empty) in one request. Simkl has no
// separate favorites concept; plan to watch is its watchlist.
func (s *Server) changeWatchlist(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, path, to string, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	var payload simklListPayload
	sent := make([]string, 0, len(events))
	for _, event := range events {
		ids := localItemIDs(event)
		if ids == (simklIDs{}) {
			results[event.GetEventId()] = rejected(event.GetEventId(), invalidRequestFault("Simkl needs an IMDb, TMDB, or TVDB id for this title"))
			continue
		}
		ref := simklListItem{To: to, IDs: ids}
		switch event.GetMedia().GetMediaType() {
		case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
			payload.Movies = append(payload.Movies, ref)
		case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES:
			payload.Shows = append(payload.Shows, ref)
		default:
			results[event.GetEventId()] = rejected(event.GetEventId(), invalidRequestFault("Simkl lists hold movies and series only"))
			continue
		}
		sent = append(sent, event.GetEventId())
	}
	return s.sendListChange(ctx, acct, path, payload, sent, results)
}

// changeDropped drops series by moving them to Simkl's "dropped" list, or
// undrops them by moving them to "watching", as the built-in provider did.
// Neither move touches watch history, and sending a move again leaves the
// show on the same list, so a redelivered event is harmless. A series with no
// id to send cannot be dropped on Simkl, so undropping it changes nothing.
func (s *Server) changeDropped(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, drop bool, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	to := "watching"
	if drop {
		to = "dropped"
	}
	var payload simklListPayload
	sent := make([]string, 0, len(events))
	for _, event := range events {
		id := event.GetEventId()
		if event.GetMedia().GetMediaType() != pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES {
			results[id] = rejected(id, invalidRequestFault("Simkl drops series only"))
			continue
		}
		ids := localItemIDs(event)
		if ids == (simklIDs{}) {
			if drop {
				results[id] = rejected(id, invalidRequestFault("Simkl needs an IMDb, TMDB, or TVDB id for this title"))
			} else {
				results[id] = noChange(id)
			}
			continue
		}
		payload.Shows = append(payload.Shows, simklListItem{To: to, IDs: ids})
		sent = append(sent, id)
	}
	return s.sendListChange(ctx, acct, "/sync/add-to-list", payload, sent, results)
}

// sendListChange posts a list write. Like the built-in provider, every title
// the request carried counts as applied.
func (s *Server) sendListChange(ctx context.Context, acct account, path string, payload simklListPayload, sent []string, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	if len(sent) == 0 {
		return nil
	}
	if _, fault := s.simkl.post(ctx, acct, path, payload, nil); fault != nil {
		return fault
	}
	for _, eventID := range sent {
		results[eventID] = applied(eventID)
	}
	return nil
}

// localItemIDs returns the ids a list or rating item is sent to Simkl with: its
// own external ids, falling back to the id its provider item key encodes.
func localItemIDs(event *pluginv1.WatchSyncEvent) simklIDs {
	external := event.GetMedia().GetExternalIds()
	ids := idsFromLocal(external["imdb"], external["tmdb"], external["tvdb"])
	if ids == (simklIDs{}) {
		ids = idsFromProviderItemKey(event.GetProviderItemKey())
	}
	return ids
}
