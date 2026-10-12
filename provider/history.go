package provider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
)

// historyTimePrecision is how finely a local play's time must match Simkl's
// to count as the same play. Simkl stores watch times to the second.
const historyTimePrecision = time.Second

// localPlay is a MARK_WATCHED or MARK_UNWATCHED event in the shape the
// built-in provider exported.
type localPlay struct {
	eventID         string
	providerItemKey string
	kind            pluginv1.WatchSyncMediaType
	title           string
	year            int
	imdbID          string
	tmdbID          string
	tvdbID          string
	seriesTitle     string
	seriesYear      int
	seriesIMDbID    string
	seriesTMDBID    string
	seriesTVDBID    string
	seasonNumber    int
	episodeNumber   int
	watchedAt       time.Time
}

func localPlayFromEvent(event *pluginv1.WatchSyncEvent) (localPlay, *pluginv1.WatchSyncFault) {
	media := event.GetMedia()
	switch media.GetMediaType() {
	case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
	default:
		return localPlay{}, invalidRequestFault("Simkl watch history holds movies and episodes only")
	}
	play := localPlay{
		eventID:       event.GetEventId(),
		kind:          media.GetMediaType(),
		title:         media.GetTitle(),
		year:          int(media.GetYear()),
		imdbID:        media.GetExternalIds()["imdb"],
		tmdbID:        media.GetExternalIds()["tmdb"],
		tvdbID:        media.GetExternalIds()["tvdb"],
		seriesTitle:   media.GetSeriesTitle(),
		seriesYear:    int(media.GetSeriesYear()),
		seriesIMDbID:  media.GetSeriesExternalIds()["imdb"],
		seriesTMDBID:  media.GetSeriesExternalIds()["tmdb"],
		seriesTVDBID:  media.GetSeriesExternalIds()["tvdb"],
		seasonNumber:  int(media.GetSeasonNumber()),
		episodeNumber: int(media.GetEpisodeNumber()),
	}
	if occurredAt := event.GetOccurredAt(); occurredAt != nil && occurredAt.CheckValid() == nil {
		play.watchedAt = occurredAt.AsTime()
	}
	play.providerItemKey = strings.TrimSpace(event.GetProviderItemKey())
	if play.providerItemKey == "" {
		play.providerItemKey = localPlayKey(play)
	}
	return play, nil
}

// localPlayKey is the key Prairie gives a local play when the event carries
// none. It mirrors the host's providerItemKeyForLocalPlay.
func localPlayKey(play localPlay) string {
	if play.kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE {
		switch {
		case play.tvdbID != "":
			return "tvdb:" + play.tvdbID
		case play.tmdbID != "":
			return "tmdb:" + play.tmdbID
		case play.seriesTVDBID != "":
			return fmt.Sprintf("show:tvdb:%s:s%d:e%d", play.seriesTVDBID, play.seasonNumber, play.episodeNumber)
		case play.seriesTMDBID != "":
			return fmt.Sprintf("show:tmdb:%s:s%d:e%d", play.seriesTMDBID, play.seasonNumber, play.episodeNumber)
		case play.seriesIMDbID != "":
			return fmt.Sprintf("show:imdb:%s:s%d:e%d", play.seriesIMDbID, play.seasonNumber, play.episodeNumber)
		}
	}
	switch {
	case play.imdbID != "":
		return "imdb:" + play.imdbID
	case play.tmdbID != "":
		return "tmdb:" + play.tmdbID
	case play.tvdbID != "":
		return "tvdb:" + play.tvdbID
	default:
		return ""
	}
}

// markWatched adds plays to the Simkl history. A play Simkl already holds at
// the same time to the second comes back NO_CHANGE without being sent again,
// which keeps redelivered events from writing twice. This is the check the
// built-in provider made against its full watched read before each export.
//
// With the profile's "Log rewatches" setting on, plays that carry a watch
// time are sent with allow_rewatch=yes, so Simkl records a play of a title
// the account already finished as a rewatch instead of ignoring it. Simkl
// merges two watches of the same title less than two days apart, so a
// redelivered play does not add a second rewatch. A play without a watch time
// is sent without the flag: Simkl would date it now, and a later redelivery
// could then count as another rewatch.
func (s *Server) markWatched(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	plays, needMovies, needEpisodes := playsFromEvents(events, results)
	if len(plays) == 0 {
		return nil
	}
	remote, fault := s.remotePlays(ctx, acct, needMovies, needEpisodes)
	if fault != nil {
		return fault
	}
	pending := make([]localPlay, 0, len(plays))
	for _, play := range plays {
		key := remotePlayKey(play.providerItemKey, play.watchedAt)
		if !play.watchedAt.IsZero() && remote[key] > 0 {
			remote[key]--
			results[play.eventID] = noChange(play.eventID)
			continue
		}
		pending = append(pending, play)
	}
	if !acct.trackRewatches {
		return s.sendHistory(ctx, acct, "/sync/history", pending, true, results)
	}
	var timed, untimed []localPlay
	for _, play := range pending {
		if play.watchedAt.IsZero() {
			untimed = append(untimed, play)
		} else {
			timed = append(timed, play)
		}
	}
	if fault := s.sendHistory(ctx, acct, "/sync/history?allow_rewatch=yes", timed, true, results); fault != nil {
		return fault
	}
	return s.sendHistory(ctx, acct, "/sync/history", untimed, true, results)
}

// markUnwatched removes plays from the Simkl history. Simkl clears the title's
// watched state; it does not address a single play.
func (s *Server) markUnwatched(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	plays, _, _ := playsFromEvents(events, results)
	return s.sendHistory(ctx, acct, "/sync/history/remove", plays, false, results)
}

func playsFromEvents(events []*pluginv1.WatchSyncEvent, results map[string]*pluginv1.WatchSyncApplyResult) (plays []localPlay, movies, episodes bool) {
	plays = make([]localPlay, 0, len(events))
	for _, event := range events {
		play, fault := localPlayFromEvent(event)
		if fault != nil {
			results[event.GetEventId()] = rejected(event.GetEventId(), fault)
			continue
		}
		plays = append(plays, play)
		if play.kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
			movies = true
		} else {
			episodes = true
		}
	}
	return plays, movies, episodes
}

// remotePlays counts the plays Simkl holds by provider item key and watch
// time. It reads the same all-items lists as a full watched import, limited to
// the kinds the batch needs.
func (s *Server) remotePlays(ctx context.Context, acct account, movies, episodes bool) (map[string]int, *pluginv1.WatchSyncFault) {
	counts := make(map[string]int)
	for _, bucket := range watchedBuckets(simklActivities{}) {
		isMovies := bucket.cursorKey == cursorInboundMoviesCompleted
		if (isMovies && !movies) || (!isMovies && !episodes) {
			continue
		}
		var payload simklAllItemsResponse
		if fault := s.simkl.get(ctx, acct, bucket.path, &payload); fault != nil {
			return nil, fault
		}
		states, _ := watchedStatesFromAllItems(payload, bucket.allowShowTimestampFallback)
		for _, state := range states {
			watchedAt := state.GetWatched().GetLastWatchedAt()
			if watchedAt == nil {
				continue
			}
			counts[remotePlayKey(state.GetProviderItemKey(), watchedAt.AsTime())]++
		}
	}
	return counts, nil
}

func remotePlayKey(providerItemKey string, watchedAt time.Time) string {
	return providerItemKey + "|" + watchedAt.UTC().Truncate(historyTimePrecision).Format(time.RFC3339)
}

// sendHistory posts plays and maps Simkl's not_found echo back to them.
func (s *Server) sendHistory(ctx context.Context, acct account, path string, plays []localPlay, includeWatchedAt bool, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	request := buildHistoryRequest(plays, includeWatchedAt)
	payload := request.payload
	if len(payload.Movies) == 0 && len(payload.Shows) == 0 && len(payload.Episodes) == 0 {
		return nil
	}
	var response simklHistoryResponse
	if _, fault := s.simkl.post(ctx, acct, path, payload, &response); fault != nil {
		return fault
	}
	notFound := make(map[string]bool)
	for _, eventID := range response.notFoundEventIDs(request.eventIDsByKey) {
		notFound[eventID] = true
	}
	for _, play := range plays {
		if notFound[play.eventID] {
			results[play.eventID] = notFoundResult(play.eventID)
			continue
		}
		results[play.eventID] = applied(play.eventID)
	}
	return nil
}

type simklHistoryRequest struct {
	payload       simklHistoryPayload
	eventIDsByKey map[string][]string
}

func buildHistoryRequest(plays []localPlay, includeWatchedAt bool) simklHistoryRequest {
	request := simklHistoryRequest{eventIDsByKey: make(map[string][]string)}
	for _, play := range plays {
		watchedAt := ""
		if includeWatchedAt && !play.watchedAt.IsZero() {
			watchedAt = play.watchedAt.UTC().Format(time.RFC3339)
		}
		switch play.kind {
		case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE:
			movie := simklHistoryMovie{
				Title:     play.title,
				Year:      play.year,
				WatchedAt: watchedAt,
				IDs:       idsFromLocal(play.imdbID, play.tmdbID, play.tvdbID),
			}
			request.payload.Movies = append(request.payload.Movies, movie)
			request.addEventID(play.eventID, historyMovieMatchKeys(movie))
		case pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE:
			show := simklHistoryShow{
				Title: play.seriesTitle,
				Year:  play.seriesYear,
				IDs:   idsFromLocal(play.seriesIMDbID, play.seriesTMDBID, play.seriesTVDBID),
				Seasons: []simklHistorySeason{{
					Number: play.seasonNumber,
					Episodes: []simklHistoryEpisode{{
						Number:    play.episodeNumber,
						WatchedAt: watchedAt,
						IDs:       idsFromLocal(play.imdbID, play.tmdbID, play.tvdbID),
					}},
				}},
			}
			request.payload.Shows = append(request.payload.Shows, show)
			request.addEventID(play.eventID, historyShowRequestMatchKeys(show))
		}
	}
	return request
}

func (r simklHistoryRequest) addEventID(eventID string, keys []string) {
	if eventID == "" {
		return
	}
	for _, key := range keys {
		if key == "" {
			continue
		}
		r.eventIDsByKey[key] = append(r.eventIDsByKey[key], eventID)
	}
}

// notFoundEventIDs maps Simkl's not_found echo, which repeats the unmatched
// entries as they were sent, back to the events that produced them.
func (r simklHistoryResponse) notFoundEventIDs(eventIDsByKey map[string][]string) []string {
	seen := make(map[string]bool)
	var eventIDs []string
	addByKeys := func(keys []string) {
		for _, key := range keys {
			for _, eventID := range eventIDsByKey[key] {
				if eventID == "" || seen[eventID] {
					continue
				}
				seen[eventID] = true
				eventIDs = append(eventIDs, eventID)
			}
		}
	}
	for _, movie := range r.NotFound.Movies {
		addByKeys(historyMovieMatchKeys(movie))
	}
	for _, show := range r.NotFound.Shows {
		addByKeys(historyShowMatchKeys(show))
	}
	for _, episode := range r.NotFound.Episodes {
		addByKeys(historyStandaloneEpisodeMatchKeys(episode))
	}
	return eventIDs
}

func historyMovieMatchKeys(movie simklHistoryMovie) []string {
	var keys []string
	watchedAt := strings.TrimSpace(movie.WatchedAt)
	for _, idKey := range historyIDMatchKeys(movie.IDs) {
		keys = appendHistoryWatchedVariants(keys, "movie:id:"+idKey, watchedAt)
	}
	if title := normalizedHistoryTitle(movie.Title); title != "" && movie.Year > 0 {
		keys = appendHistoryWatchedVariants(keys, fmt.Sprintf("movie:title:%s:%d", title, movie.Year), watchedAt)
	}
	return keys
}

func historyShowMatchKeys(show simklHistoryShow) []string {
	if len(show.Seasons) == 0 {
		return historyShowOnlyMatchKeys(show)
	}
	return historyShowEpisodeMatchKeys(show)
}

func historyShowRequestMatchKeys(show simklHistoryShow) []string {
	keys := historyShowOnlyMatchKeys(show)
	return append(keys, historyShowEpisodeMatchKeys(show)...)
}

func historyShowOnlyMatchKeys(show simklHistoryShow) []string {
	var keys []string
	for _, showKey := range historyShowIdentityKeys(show) {
		keys = append(keys, "show:"+showKey)
	}
	return keys
}

func historyShowEpisodeMatchKeys(show simklHistoryShow) []string {
	var keys []string
	showKeys := historyShowIdentityKeys(show)
	for _, season := range show.Seasons {
		for _, episode := range season.Episodes {
			for _, showKey := range showKeys {
				keys = append(keys, historyShowEpisodeIdentityKeys(showKey, season.Number, episode)...)
			}
			keys = append(keys, historyStandaloneEpisodeMatchKeys(episode)...)
		}
	}
	return keys
}

func historyShowIdentityKeys(show simklHistoryShow) []string {
	var keys []string
	for _, idKey := range historyIDMatchKeys(show.IDs) {
		keys = append(keys, "id:"+idKey)
	}
	if title := normalizedHistoryTitle(show.Title); title != "" && show.Year > 0 {
		keys = append(keys, fmt.Sprintf("title:%s:%d", title, show.Year))
	}
	return keys
}

func historyShowEpisodeIdentityKeys(showKey string, seasonNumber int, episode simklHistoryEpisode) []string {
	base := fmt.Sprintf("episode:show:%s:s%d:e%d", showKey, seasonNumber, episode.Number)
	return appendHistoryWatchedVariants(nil, base, strings.TrimSpace(episode.WatchedAt))
}

func historyStandaloneEpisodeMatchKeys(episode simklHistoryEpisode) []string {
	var keys []string
	watchedAt := strings.TrimSpace(episode.WatchedAt)
	for _, idKey := range historyIDMatchKeys(episode.IDs) {
		keys = appendHistoryWatchedVariants(keys, "episode:id:"+idKey, watchedAt)
	}
	return keys
}

func appendHistoryWatchedVariants(keys []string, base string, watchedAt string) []string {
	if base == "" {
		return keys
	}
	if watchedAt != "" {
		keys = append(keys, base+":watched:"+watchedAt)
	}
	return append(keys, base)
}

func historyIDMatchKeys(ids simklIDs) []string {
	keys := make([]string, 0, 4)
	if ids.IMDb != "" {
		keys = append(keys, "imdb:"+ids.IMDb)
	}
	if ids.TMDB > 0 {
		keys = append(keys, "tmdb:"+strconv.Itoa(ids.TMDB))
	}
	if ids.TVDB > 0 {
		keys = append(keys, "tvdb:"+strconv.Itoa(ids.TVDB))
	}
	if ids.Simkl > 0 {
		keys = append(keys, "simkl:"+strconv.Itoa(ids.Simkl))
	}
	return keys
}

func normalizedHistoryTitle(title string) string {
	return strings.Join(strings.Fields(strings.ToLower(title)), " ")
}
