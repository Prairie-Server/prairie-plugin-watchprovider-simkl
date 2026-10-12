package provider

import (
	"context"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Cursor keys. A traversal's cursor is a JSON object of these keys and the
// /sync/activities timestamps last read for them, the same per-list state the
// built-in provider stored under the same names.
const (
	cursorInboundMoviesCompleted = "simkl.inbound.movies.completed"
	cursorInboundShowsWatching   = "simkl.inbound.shows.watching"
	cursorInboundShowsCompleted  = "simkl.inbound.shows.completed"
	cursorInboundAnimeWatching   = "simkl.inbound.anime.watching"
	cursorInboundAnimeCompleted  = "simkl.inbound.anime.completed"
	cursorProgressMovies         = "simkl.progress.movies"
	cursorProgressShows          = "simkl.progress.shows"
	cursorProgressAnime          = "simkl.progress.anime"

	// A removal on Simkl moves a list's removed_from_list stamp. The watched
	// import cannot apply removals, so a moved stamp is reported as a warning.
	cursorRemovedMovies = "simkl.inbound.movies.removed_from_list"
	cursorRemovedShows  = "simkl.inbound.shows.removed_from_list"
	cursorRemovedAnime  = "simkl.inbound.anime.removed_from_list"

	cursorDroppedShows = "simkl.dropped.shows"
	cursorDroppedAnime = "simkl.dropped.anime"
)

// Import warnings, worded as the built-in provider worded them.
const (
	warnRemovedFromList     = "simkl removed_from_list changed; removals are not imported"
	warnWatchedMovieNoID    = "simkl watched movie skipped because it has no usable external id"
	warnWatchedEpisodeNoID  = "simkl watched episode skipped because it has no usable external id path"
	warnPlaybackMovieNoID   = "simkl playback movie skipped because it has no usable external id"
	warnPlaybackEpisodeNoID = "simkl playback episode skipped because it has no usable external id path"
)

// What a traversal step reads.
const (
	readWatched         = "watched"
	readProgress        = "progress"
	readWatchlistMovies = "watchlist_movies"
	readWatchlistShows  = "watchlist_shows"
	readRatings         = "ratings"
	readDropped         = "dropped"
)

// maxProgressPercent keeps a resume point inside the contract's [0, 100)
// range. The built-in provider passed Simkl's value through, and the host
// stored 100 as a resume point at the very end, which this matches.
const maxProgressPercent = 99.999

// step is one upstream read of a traversal.
type step struct {
	Read string `json:"read"`
	Path string `json:"path,omitempty"`
	// Fallback lets a completed show's last_watched_at date its episodes that
	// have no watched_at of their own.
	Fallback bool `json:"fallback,omitempty"`
	// AnimeCursor is the anime playback stamp to record if the read returns
	// anime rows.
	AnimeCursor string `json:"anime_cursor,omitempty"`
}

// stepRead is what one step read returned.
type stepRead struct {
	rows      []*pluginv1.WatchSyncRemoteState
	complete  bool
	animeRows bool
	warnings  []string
}

type watchedBucket struct {
	cursorKey                  string
	activity                   string
	path                       string
	allowShowTimestampFallback bool
}

func watchedBuckets(activities simklActivities) []watchedBucket {
	return []watchedBucket{
		{
			cursorKey: cursorInboundMoviesCompleted,
			activity:  activities.Movies.Completed,
			path:      "/sync/all-items/movies/completed?extended=full&episode_watched_at=yes",
		},
		{
			cursorKey: cursorInboundShowsWatching,
			activity:  activities.TVShows.Watching,
			path:      "/sync/all-items/shows/watching?extended=full&episode_watched_at=yes",
		},
		{
			cursorKey:                  cursorInboundShowsCompleted,
			activity:                   activities.TVShows.Completed,
			path:                       "/sync/all-items/shows/completed?extended=full&episode_watched_at=yes",
			allowShowTimestampFallback: true,
		},
		{
			cursorKey: cursorInboundAnimeWatching,
			activity:  activities.Anime.Watching,
			path:      "/sync/all-items/anime/watching?extended=full_anime_seasons&episode_watched_at=yes",
		},
		{
			cursorKey:                  cursorInboundAnimeCompleted,
			activity:                   activities.Anime.Completed,
			path:                       "/sync/all-items/anime/completed?extended=full_anime_seasons&episode_watched_at=yes",
			allowShowTimestampFallback: true,
		},
	}
}

// planTraversal reads /sync/activities and lists the reads a traversal needs.
// Lists whose activity matches the cursor are skipped, and changed lists are
// read from the cursor's timestamp on, exactly as the built-in provider did.
// An empty cursor reads everything.
func (s *Server) planTraversal(ctx context.Context, acct account, kind pluginv1.WatchSyncRemoteStateKind, cursor string) (*pageToken, *pluginv1.WatchSyncFault) {
	previous := decodeCursor(cursor)
	token := &pageToken{Kind: int32(kind), Cursor: cursor, Next: cloneMap(previous)}
	switch kind {
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHED:
		activities, fault := s.activities(ctx, acct)
		if fault != nil {
			return nil, fault
		}
		for _, removed := range []struct{ key, activity string }{
			{cursorRemovedMovies, activities.Movies.RemovedFromList},
			{cursorRemovedShows, activities.TVShows.RemovedFromList},
			{cursorRemovedAnime, activities.Anime.RemovedFromList},
		} {
			if removed.activity == "" || previous[removed.key] == removed.activity {
				continue
			}
			token.warn(warnRemovedFromList)
			token.Next[removed.key] = removed.activity
		}
		for _, bucket := range watchedBuckets(activities) {
			last := previous[bucket.cursorKey]
			if shouldSkipSimklBucket(last, bucket.activity) {
				continue
			}
			token.Steps = append(token.Steps, step{
				Read:     readWatched,
				Path:     appendDateFrom(bucket.path, last),
				Fallback: bucket.allowShowTimestampFallback,
			})
			if bucket.activity != "" {
				token.Next[bucket.cursorKey] = bucket.activity
			}
		}
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_PROGRESS:
		activities, fault := s.activities(ctx, acct)
		if fault != nil {
			return nil, fault
		}
		token.Steps = progressSteps(activities, previous, token.Next)
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_WATCHLIST:
		// Like the built-in provider, every watchlist read is a full read and
		// an authoritative snapshot, so it needs no cursor.
		token.Next = nil
		token.Complete = true
		token.Steps = []step{
			{Read: readWatchlistMovies, Path: "/sync/all-items/movies/plantowatch?extended=full"},
			{Read: readWatchlistShows, Path: "/sync/all-items/shows/plantowatch?extended=full"},
			{Read: readWatchlistShows, Path: "/sync/all-items/anime/plantowatch?extended=full"},
		}
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_RATING:
		activities, fault := s.activities(ctx, acct)
		if fault != nil {
			return nil, fault
		}
		stamps := map[string]string{
			cursorRatingsMovies: activities.Movies.RatedAt,
			cursorRatingsShows:  activities.TVShows.RatedAt,
			cursorRatingsAnime:  activities.Anime.RatedAt,
		}
		changed := false
		for key, activity := range stamps {
			changed = changed || ratingsChanged(previous[key], activity)
		}
		if changed {
			// Every list is read, so a changed list of either kind yields a
			// snapshot of both; see readRatings.
			token.Steps = []step{{Read: readRatings}}
			for key, activity := range stamps {
				if activity != "" {
					token.Next[key] = activity
				}
			}
		}
	case pluginv1.WatchSyncRemoteStateKind_WATCH_SYNC_REMOTE_STATE_KIND_DROPPED:
		activities, fault := s.activities(ctx, acct)
		if fault != nil {
			return nil, fault
		}
		planDropped(token, activities, previous)
	default:
		return nil, invalidRequestFault("Simkl does not support the requested state family")
	}
	return token, nil
}

// planDropped plans the dropped-shows read. It checks each list's "all" stamp
// rather than its "dropped" one, so a show moved out of the dropped list is
// never missed. While neither stamp moved, the read is skipped and is no
// snapshot. Otherwise both lists are read in full, never with date_from, which
// could not show an undrop, so the read is the account's complete dropped set.
func planDropped(token *pageToken, activities simklActivities, previous map[string]string) {
	stamps := []struct{ key, activity string }{
		{cursorDroppedShows, activities.TVShows.All},
		{cursorDroppedAnime, activities.Anime.All},
	}
	changed := false
	for _, stamp := range stamps {
		changed = changed || !shouldSkipSimklBucket(previous[stamp.key], stamp.activity)
	}
	if !changed {
		return
	}
	// Like the built-in provider, a read records both stamps as they are, so
	// a list without a stamp is read again on the next sync.
	for _, stamp := range stamps {
		if stamp.activity == "" {
			delete(token.Next, stamp.key)
			continue
		}
		token.Next[stamp.key] = stamp.activity
	}
	token.Complete = true
	token.Steps = []step{
		{Read: readDropped, Path: "/sync/all-items/shows/dropped?extended=full"},
		{Read: readDropped, Path: "/sync/all-items/anime/dropped?extended=full"},
	}
}

// progressSteps plans the playback reads and records the stamps they settle
// in next. Shows and anime share one episode playback read, which starts at
// the older of their two stamps, or at the beginning when a changed one of
// them was never read. The anime stamp is recorded only if that read returns
// anime rows; see readStep.
func progressSteps(activities simklActivities, previous, next map[string]string) []step {
	var steps []step
	moviePrevious := previous[cursorProgressMovies]
	if !shouldSkipSimklBucket(moviePrevious, activities.Movies.Playback) {
		steps = append(steps, step{Read: readProgress, Path: appendDateFrom("/sync/playback/movies", moviePrevious)})
		if activities.Movies.Playback != "" {
			next[cursorProgressMovies] = activities.Movies.Playback
		}
	}
	showsPrevious := previous[cursorProgressShows]
	animePrevious := previous[cursorProgressAnime]
	showsChanged := !shouldSkipSimklBucket(showsPrevious, activities.TVShows.Playback)
	animeChanged := !shouldSkipSimklBucket(animePrevious, activities.Anime.Playback)
	if !showsChanged && !animeChanged {
		return steps
	}
	dateFrom := ""
	freshShows := showsChanged && showsPrevious == ""
	freshAnime := animeChanged && animePrevious == ""
	if !freshShows && !freshAnime {
		dateFrom = oldestCursor(showsPrevious, animePrevious)
	}
	episodes := step{Read: readProgress, Path: appendDateFrom("/sync/playback/episodes", dateFrom)}
	if showsChanged && activities.TVShows.Playback != "" {
		next[cursorProgressShows] = activities.TVShows.Playback
	}
	if animeChanged && activities.Anime.Playback != "" {
		episodes.AnimeCursor = activities.Anime.Playback
	}
	return append(steps, episodes)
}

func (s *Server) activities(ctx context.Context, acct account) (simklActivities, *pluginv1.WatchSyncFault) {
	var activities simklActivities
	if fault := s.simkl.get(ctx, acct, "/sync/activities", &activities); fault != nil {
		return simklActivities{}, fault
	}
	return activities, nil
}

// readStep performs one step's upstream read.
func (s *Server) readStep(ctx context.Context, acct account, current step) (stepRead, *pluginv1.WatchSyncFault) {
	switch current.Read {
	case readWatched:
		var payload simklAllItemsResponse
		if fault := s.simkl.get(ctx, acct, current.Path, &payload); fault != nil {
			return stepRead{}, fault
		}
		rows, warnings := watchedStatesFromAllItems(payload, current.Fallback)
		return stepRead{rows: rows, warnings: warnings}, nil
	case readProgress:
		var payload []simklPlayback
		if fault := s.simkl.get(ctx, acct, current.Path, &payload); fault != nil {
			return stepRead{}, fault
		}
		rows, animeRows, warnings := progressStatesFromPlayback(payload)
		return stepRead{rows: rows, animeRows: animeRows, warnings: warnings}, nil
	case readWatchlistMovies, readWatchlistShows:
		var payload simklAllItemsResponse
		if fault := s.simkl.get(ctx, acct, current.Path, &payload); fault != nil {
			return stepRead{}, fault
		}
		return stepRead{rows: watchlistStates(payload)}, nil
	case readRatings:
		rows, complete, warnings, fault := s.readRatings(ctx, acct)
		if fault != nil {
			return stepRead{}, fault
		}
		return stepRead{rows: rows, complete: complete, warnings: warnings}, nil
	case readDropped:
		var payload simklAllItemsResponse
		if fault := s.simkl.get(ctx, acct, current.Path, &payload); fault != nil {
			return stepRead{}, fault
		}
		return stepRead{rows: droppedStates(payload)}, nil
	default:
		return stepRead{}, invalidRequestFault("Simkl page token is invalid")
	}
}

func shouldSkipSimklBucket(previous string, activity string) bool {
	if previous == "" {
		return false
	}
	if strings.TrimSpace(activity) == "" {
		return true
	}
	return previous == activity
}

func oldestCursor(values ...string) string {
	var oldest string
	var oldestTime time.Time
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			if oldest == "" {
				oldest = value
			}
			continue
		}
		if oldest == "" || parsed.Before(oldestTime) {
			oldest = value
			oldestTime = parsed
		}
	}
	return oldest
}

// watchedStatesFromAllItems maps an all-items read to watched movies and
// episodes. Simkl reports the last watch only, so each title counts one play.
// Rows without a usable id are skipped with a warning each.
func watchedStatesFromAllItems(payload simklAllItemsResponse, allowShowTimestampFallback bool) (states []*pluginv1.WatchSyncRemoteState, warnings []string) {
	states = make([]*pluginv1.WatchSyncRemoteState, 0, len(payload.Movies))
	for _, movie := range payload.Movies {
		if movie.Status != "" && movie.Status != "completed" {
			continue
		}
		if movie.LastWatchedAt == nil {
			continue
		}
		key := movieKey(movie.Movie.IDs)
		if key == "" {
			warnings = append(warnings, warnWatchedMovieNoID)
			continue
		}
		states = append(states, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media:           movieMedia(movie.Movie),
			Watched:         &pluginv1.WatchSyncRemoteWatchedState{PlayCount: 1, LastWatchedAt: timestamppb.New(*movie.LastWatchedAt)},
		})
	}
	for _, shows := range [][]simklShowItem{payload.Shows, payload.Anime} {
		for _, show := range shows {
			for _, season := range show.Seasons {
				for _, episode := range season.Episodes {
					watchedAt := episode.WatchedAt
					if watchedAt == nil && allowShowTimestampFallback && show.Status == "completed" {
						watchedAt = show.LastWatchedAt
					}
					if watchedAt == nil {
						continue
					}
					seasonNumber, number := episodeNumbers(episode, season.Number)
					key := episodeKey(show.Show.IDs, seasonNumber, number, episode.IDs)
					if key == "" {
						warnings = append(warnings, warnWatchedEpisodeNoID)
						continue
					}
					media := episodeMedia(show.Show, episode.Title, seasonNumber, number)
					media.ExternalIds = externalIDs(episode.IDs)
					states = append(states, &pluginv1.WatchSyncRemoteState{
						ProviderItemKey: key,
						Media:           media,
						Watched:         &pluginv1.WatchSyncRemoteWatchedState{PlayCount: 1, LastWatchedAt: timestamppb.New(*watchedAt)},
					})
				}
			}
		}
	}
	return states, warnings
}

// progressStatesFromPlayback maps paused playback. animeRows reports whether
// any row came from an anime title. Episode rows carry the show's ids only,
// as the built-in provider's did. Rows without a usable id are skipped with a
// warning each.
func progressStatesFromPlayback(payload []simklPlayback) (states []*pluginv1.WatchSyncRemoteState, animeRows bool, warnings []string) {
	states = make([]*pluginv1.WatchSyncRemoteState, 0, len(payload))
	for _, item := range payload {
		progress := &pluginv1.WatchSyncRemoteProgressState{
			ProgressPercent: min(max(item.Progress, 0), maxProgressPercent),
			PausedAt:        timestamppb.New(item.PausedAt),
		}
		switch item.Type {
		case "movie":
			key := movieKey(item.Movie.IDs)
			if key == "" {
				warnings = append(warnings, warnPlaybackMovieNoID)
				continue
			}
			states = append(states, &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: key,
				Media:           movieMedia(item.Movie),
				Progress:        progress,
			})
		case "episode", "show", "anime":
			show := item.Show
			if item.Type == "anime" || show.Title == "" && item.Anime.Title != "" {
				show = item.Anime
				animeRows = true
			}
			season, episode := episodeNumbers(item.Episode, 0)
			key := episodeKey(show.IDs, season, episode, item.Episode.IDs)
			if key == "" {
				warnings = append(warnings, warnPlaybackEpisodeNoID)
				continue
			}
			states = append(states, &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: key,
				Media:           episodeMedia(show, item.Episode.Title, season, episode),
				Progress:        progress,
			})
		}
	}
	return states, animeRows, warnings
}

// watchlistStates maps a plan-to-watch read. Movies are movies; shows and
// anime are series. Simkl reports no time a title was added, so listed_at is
// left for the host to fill in.
func watchlistStates(payload simklAllItemsResponse) []*pluginv1.WatchSyncRemoteState {
	var states []*pluginv1.WatchSyncRemoteState
	for _, movie := range payload.Movies {
		key := movieKey(movie.Movie.IDs)
		if key == "" {
			continue
		}
		states = append(states, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media:           movieMedia(movie.Movie),
			Watchlist:       &pluginv1.WatchSyncRemoteListState{},
		})
	}
	for _, shows := range [][]simklShowItem{payload.Shows, payload.Anime} {
		for _, show := range shows {
			key := showKey(show.Show.IDs)
			if key == "" {
				continue
			}
			states = append(states, &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: key,
				Media:           seriesMedia(show.Show),
				Watchlist:       &pluginv1.WatchSyncRemoteListState{},
			})
		}
	}
	return states
}

// droppedStates maps a dropped-list read of shows or anime to dropped series.
// Simkl records no time a show was dropped, so listed_at is left for the host
// to fill in. Like the built-in provider, a show without any id is skipped.
func droppedStates(payload simklAllItemsResponse) []*pluginv1.WatchSyncRemoteState {
	var states []*pluginv1.WatchSyncRemoteState
	for _, shows := range [][]simklShowItem{payload.Shows, payload.Anime} {
		for _, show := range shows {
			key := showKey(show.Show.IDs)
			if key == "" {
				continue
			}
			states = append(states, &pluginv1.WatchSyncRemoteState{
				ProviderItemKey: key,
				Media:           seriesMedia(show.Show),
				Dropped:         &pluginv1.WatchSyncRemoteListState{},
			})
		}
	}
	return states
}

func movieMedia(movie simklMovie) *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE,
		Title:       movie.Title,
		Year:        int32(movie.Year),
		ExternalIds: externalIDs(movie.IDs),
	}
}

func seriesMedia(show simklShow) *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaType:   pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES,
		Title:       show.Title,
		Year:        int32(show.Year),
		ExternalIds: externalIDs(show.IDs),
	}
}

func episodeMedia(show simklShow, title string, season, episode int) *pluginv1.WatchSyncMedia {
	return &pluginv1.WatchSyncMedia{
		MediaType:         pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_EPISODE,
		Title:             title,
		SeriesTitle:       show.Title,
		SeriesYear:        int32(show.Year),
		SeriesExternalIds: externalIDs(show.IDs),
		SeasonNumber:      int32(season),
		EpisodeNumber:     int32(episode),
	}
}

// externalIDs returns the IMDb, TMDB, and TVDB ids the host matches on.
func externalIDs(ids simklIDs) map[string]string {
	external := make(map[string]string, 3)
	if ids.IMDb != "" {
		external["imdb"] = ids.IMDb
	}
	if value := intString(ids.TMDB); value != "" {
		external["tmdb"] = value
	}
	if value := intString(ids.TVDB); value != "" {
		external["tvdb"] = value
	}
	if len(external) == 0 {
		return nil
	}
	return external
}
