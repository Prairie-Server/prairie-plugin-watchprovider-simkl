package provider

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	pluginv1 "github.com/prairie-server/prairie-plugin-sdk/pkg/pluginproto/prairie/plugin/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Simkl rates on the integer 1 to 10 scale the plugin contract uses, so
// ratings pass through unchanged. Only movie and show ratings exist on Simkl;
// Prairie rates movies and series, so nothing else is read or written.
//
// Simkl keeps anime, anime movies included, in a separate anime domain with its
// own rated_at activity. Anime movies are movies to Prairie and every other anime
// entry is a series (see animeRatingIdentity), so the anime ratings belong to
// both Prairie kinds.

const (
	cursorRatingsMovies = "simkl.ratings.movies"
	cursorRatingsShows  = "simkl.ratings.shows"
	cursorRatingsAnime  = "simkl.ratings.anime"

	// Simkl's media type path segments.
	simklTypeMovies = "movies"
	simklTypeShows  = "shows"
	simklTypeAnime  = "anime"

	// The anime_type values that name a Prairie kind outright.
	simklAnimeTypeMovie = "movie"
	simklAnimeTypeTV    = "tv"

	// simklEveryRating is the rating filter of a ratings read. Listing every
	// value returns only rated items; without a filter Simkl returns the
	// whole library, unrated items included.
	simklEveryRating = "1,2,3,4,5,6,7,8,9,10"

	// A traversal is a complete snapshot of both kinds or of neither, so
	// either warning means no rating removal of either kind is imported.
	warnUntypedAnimeRating = "simkl returned rated anime without a movie or tv type; skipped movie and series rating removals"
	warnRatingNoIDFormat   = "simkl %s rating skipped because it has no usable id; skipped movie and series rating removals"
)

// simklRatingsList is the reply to GET /sync/ratings/{type}/{rating}. Each
// read fills only the key of its type, and an account with no ratings of that
// type gets {}. Anime entries wrap their title in "show".
type simklRatingsList struct {
	Movies []simklRatedItem `json:"movies"`
	Shows  []simklRatedItem `json:"shows"`
	Anime  []simklRatedItem `json:"anime"`
}

type simklRatedItem struct {
	UserRating  *float64   `json:"user_rating"`
	UserRatedAt string     `json:"user_rated_at"`
	AnimeType   string     `json:"anime_type"`
	Movie       simklMovie `json:"movie"`
	Show        simklShow  `json:"show"`
}

// simklRatingItem is one entry of a ratings write or removal. A removal sends
// ids only.
type simklRatingItem struct {
	Rating  int      `json:"rating,omitempty"`
	RatedAt string   `json:"rated_at,omitempty"`
	IDs     simklIDs `json:"ids"`
}

// simklRatingsPayload sends Prairie series as shows, anime included: Simkl
// resolves an anime title under shows as well as under anime.
type simklRatingsPayload struct {
	Movies []simklRatingItem `json:"movies,omitempty"`
	Shows  []simklRatingItem `json:"shows,omitempty"`
}

// simklRatingsWriteResponse is the reply to POST /sync/ratings and
// /sync/ratings/remove. The not_found lists echo the items Simkl could not
// match as they were sent, with anime folded into shows.
type simklRatingsWriteResponse struct {
	NotFound struct {
		Movies []simklRatingItem `json:"movies"`
		Shows  []simklRatingItem `json:"shows"`
	} `json:"not_found"`
}

// ratingsChanged reports whether a rated_at activity moved since the cursor.
// A null activity means the account never rated that type, which is no
// change: shouldSkipSimklBucket alone would re-read such a bucket, and the
// kinds that share it, on every run.
func ratingsChanged(previous, activity string) bool {
	if strings.TrimSpace(activity) == "" {
		return false
	}
	return !shouldSkipSimklBucket(previous, activity)
}

// readRatings reads the movie, show, and anime rating lists in full, never
// with date_from: a delta cannot show a removed rating. complete reports
// whether the rows are every rating of both Prairie kinds.
//
// The built-in provider could declare movies and series complete separately.
// A plugin traversal is complete for both kinds or neither, so the rows are a
// complete snapshot only when the built-in would have declared both kinds.
func (s *Server) readRatings(ctx context.Context, acct account) (states []*pluginv1.WatchSyncRemoteState, complete bool, warnings []string, fault *pluginv1.WatchSyncFault) {
	anyUntyped := false
	skipped := make(map[pluginv1.WatchSyncMediaType]bool)
	for _, listType := range []string{simklTypeMovies, simklTypeShows, simklTypeAnime} {
		var list simklRatingsList
		if fault := s.simkl.get(ctx, acct, "/sync/ratings/"+listType+"/"+simklEveryRating, &list); fault != nil {
			return nil, false, nil, fault
		}
		rows, untyped, skippedKinds, listWarnings := ratingStatesFromList(list, listType)
		states = append(states, rows...)
		warnings = append(warnings, listWarnings...)
		anyUntyped = anyUntyped || untyped
		for kind := range skippedKinds {
			skipped[kind] = true
		}
	}
	// An anime entry without a movie or tv type is read as a series, but it
	// may be an anime movie, which the movie read never returns. The movie read
	// is then not provably complete. A rated entry skipped for lack of an id
	// is a title the read did not return, so its kind is not complete either.
	if anyUntyped {
		warnings = append(warnings, warnUntypedAnimeRating)
	}
	complete = !anyUntyped &&
		!skipped[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE] &&
		!skipped[pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES]
	return states, complete, warnings, nil
}

// ratingStatesFromList maps one ratings read. listType is the type the read
// asked for; only that key of the reply is used. untyped reports a rated anime
// entry whose anime_type names neither a movie nor a series. skippedKinds holds
// the kind of each rated entry skipped for lack of a usable id, and warnings
// has one warning per such entry.
func ratingStatesFromList(list simklRatingsList, listType string) (states []*pluginv1.WatchSyncRemoteState, untyped bool, skippedKinds map[pluginv1.WatchSyncMediaType]bool, warnings []string) {
	var items []simklRatedItem
	switch listType {
	case simklTypeMovies:
		items = list.Movies
	case simklTypeShows:
		items = list.Shows
	case simklTypeAnime:
		items = list.Anime
	}
	states = make([]*pluginv1.WatchSyncRemoteState, 0, len(items))
	skippedKinds = make(map[pluginv1.WatchSyncMediaType]bool)
	for _, item := range items {
		if item.UserRating == nil {
			continue
		}
		var (
			kind  pluginv1.WatchSyncMediaType
			title string
			year  int
			ids   simklIDs
		)
		switch listType {
		case simklTypeMovies:
			kind, title, year, ids = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, item.Movie.Title, item.Movie.Year, item.Movie.IDs
		case simklTypeAnime:
			kind, ids = animeRatingIdentity(item.AnimeType, item.Show.IDs)
			untyped = untyped || !typedAnime(item.AnimeType)
			title, year = item.Show.Title, item.Show.Year
		default:
			kind, title, year, ids = pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES, item.Show.Title, item.Show.Year, item.Show.IDs
		}
		key := showKey(ids)
		if kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
			key = movieKey(ids)
		}
		if key == "" {
			skippedKinds[kind] = true
			warnings = append(warnings, fmt.Sprintf(warnRatingNoIDFormat, kindName(kind)))
			continue
		}
		rating := &pluginv1.WatchSyncRemoteRatingState{Rating: int32(min(max(math.Round(*item.UserRating), 1), 10))}
		if ratedAt := parseSimklTime(item.UserRatedAt); !ratedAt.IsZero() {
			rating.RatedAt = timestamppb.New(ratedAt)
		}
		states = append(states, &pluginv1.WatchSyncRemoteState{
			ProviderItemKey: key,
			Media: &pluginv1.WatchSyncMedia{
				MediaType:   kind,
				Title:       title,
				Year:        int32(year),
				ExternalIds: externalIDs(ids),
			},
			Rating: rating,
		})
	}
	return states, untyped, skippedKinds, warnings
}

// kindName names a rateable media type the way the built-in provider's
// warnings did.
func kindName(kind pluginv1.WatchSyncMediaType) string {
	if kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		return "movie"
	}
	return "series"
}

// typedAnime reports whether an anime_type names a Prairie kind outright.
func typedAnime(animeType string) bool {
	switch strings.ToLower(strings.TrimSpace(animeType)) {
	case simklAnimeTypeMovie, simklAnimeTypeTV:
		return true
	default:
		return false
	}
}

// animeRatingIdentity returns the Prairie kind and the ids of a rated anime entry
// from its anime_type. A "movie" is a movie and a "tv" entry a series, each
// with every id. Any other type (ova, ona, special, music video) or a missing
// one is a series without its TMDB id: such an entry can carry a TMDB movie
// id, and TMDB numbers movies and series separately, so that id could match an
// unrelated series. The IMDb and TVDB ids are kept.
//
// Simkl's OpenAPI spec does not list anime_type on GET /sync/ratings reads;
// the documented anime entry has only the rating fields, status, and show.
// GET /sync/all-items documents it on every anime entry as nullable (tv,
// movie, ova, ona, special, music video). A ratings read without it is
// therefore expected and takes the missing-type path.
func animeRatingIdentity(animeType string, ids simklIDs) (pluginv1.WatchSyncMediaType, simklIDs) {
	switch strings.ToLower(strings.TrimSpace(animeType)) {
	case simklAnimeTypeMovie:
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, ids
	case simklAnimeTypeTV:
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES, ids
	default:
		ids.TMDB = 0
		return pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES, ids
	}
}

// parseSimklTime parses a Simkl timestamp. A missing or malformed one is the
// zero time, which the host treats as unknown.
func parseSimklTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

// ratingRef is a rating event a write carried, for mapping not_found back.
type ratingRef struct {
	eventID string
	kind    pluginv1.WatchSyncMediaType
	ids     simklIDs
}

// setRatings sets movie and series ratings in one batch. Simkl overwrites an
// existing rating, so resending one is harmless.
//
// Rating a released movie that is not on the user's Simkl list files it as
// completed, which records it as watched, so the manifest lists movies in
// rating_export_requires_watched and the host sends a movie rating only after
// the profile has watched the movie. Rating an unlisted show files it as
// watching with no episodes marked, except that a single-episode show is filed
// as completed; series are not held back for that case.
func (s *Server) setRatings(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	var payload simklRatingsPayload
	refs := make([]ratingRef, 0, len(events))
	for _, event := range events {
		id := event.GetEventId()
		if event.GetRating() < 1 || event.GetRating() > 10 {
			results[id] = rejected(id, invalidRequestFault("Simkl ratings run from 1 to 10"))
			continue
		}
		kind := event.GetMedia().GetMediaType()
		if !rateable(kind) {
			results[id] = rejected(id, invalidRequestFault("Simkl rates movies and series only"))
			continue
		}
		ids := localItemIDs(event)
		if ids == (simklIDs{}) {
			results[id] = rejected(id, invalidRequestFault("Simkl needs an IMDb, TMDB, or TVDB id for this title"))
			continue
		}
		entry := simklRatingItem{Rating: int(event.GetRating()), IDs: ids}
		if ratedAt := event.GetOccurredAt(); ratedAt != nil && ratedAt.CheckValid() == nil && !ratedAt.AsTime().IsZero() {
			entry.RatedAt = ratedAt.AsTime().UTC().Format(time.RFC3339)
		}
		payload.add(kind, entry)
		refs = append(refs, ratingRef{eventID: id, kind: kind, ids: ids})
	}
	return s.sendRatings(ctx, acct, "/sync/ratings", payload, refs, notFoundResult, results)
}

// removeRatings clears movie and series ratings in one batch. The title stays
// on the user's Simkl list. A title Simkl cannot match, or that has no id to
// send, has no rating to clear, so it counts as cleared.
func (s *Server) removeRatings(ctx context.Context, acct account, events []*pluginv1.WatchSyncEvent, results map[string]*pluginv1.WatchSyncApplyResult) *pluginv1.WatchSyncFault {
	var payload simklRatingsPayload
	refs := make([]ratingRef, 0, len(events))
	for _, event := range events {
		id := event.GetEventId()
		kind := event.GetMedia().GetMediaType()
		if !rateable(kind) {
			results[id] = rejected(id, invalidRequestFault("Simkl rates movies and series only"))
			continue
		}
		ids := localItemIDs(event)
		if ids == (simklIDs{}) {
			// Simkl cannot hold a rating for a title it could never match.
			results[id] = noChange(id)
			continue
		}
		payload.add(kind, simklRatingItem{IDs: ids})
		refs = append(refs, ratingRef{eventID: id, kind: kind, ids: ids})
	}
	return s.sendRatings(ctx, acct, "/sync/ratings/remove", payload, refs, noChange, results)
}

func rateable(kind pluginv1.WatchSyncMediaType) bool {
	return kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE || kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES
}

// add appends entry under its kind, a movie or a series.
func (payload *simklRatingsPayload) add(kind pluginv1.WatchSyncMediaType, entry simklRatingItem) {
	if kind == pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE {
		payload.Movies = append(payload.Movies, entry)
		return
	}
	payload.Shows = append(payload.Shows, entry)
}

// sendRatings posts a ratings payload and maps the reply back to refs, the
// entries payload holds. A ref whose kind matches a not_found echo sharing any
// of its ids gets missing's result; every other ref is applied.
func (s *Server) sendRatings(
	ctx context.Context,
	acct account,
	path string,
	payload simklRatingsPayload,
	refs []ratingRef,
	missing func(string) *pluginv1.WatchSyncApplyResult,
	results map[string]*pluginv1.WatchSyncApplyResult,
) *pluginv1.WatchSyncFault {
	if len(refs) == 0 {
		return nil
	}
	var response simklRatingsWriteResponse
	if _, fault := s.simkl.post(ctx, acct, path, payload, &response); fault != nil {
		return fault
	}
	notFound := simklIDIndex{}
	for _, movie := range response.NotFound.Movies {
		notFound.add(pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_MOVIE, movie.IDs)
	}
	for _, show := range response.NotFound.Shows {
		notFound.add(pluginv1.WatchSyncMediaType_WATCH_SYNC_MEDIA_TYPE_SERIES, show.IDs)
	}
	for _, ref := range refs {
		if notFound.matches(ref.kind, ref.ids) {
			results[ref.eventID] = missing(ref.eventID)
			continue
		}
		results[ref.eventID] = applied(ref.eventID)
	}
	return nil
}

// simklIDIndex matches the items Simkl echoes in a not_found list to the
// request items that produced them. An echo matches an item of the same kind
// when the two share ANY id, so an echo that carries a different id subset
// than the item's preferred key still matches. Ids are qualified by kind
// because TMDB and TVDB number movies and series separately.
type simklIDIndex map[string]struct{}

func (idx simklIDIndex) add(kind pluginv1.WatchSyncMediaType, ids simklIDs) {
	for _, key := range historyIDMatchKeys(ids) {
		idx[kind.String()+":"+key] = struct{}{}
	}
}

func (idx simklIDIndex) matches(kind pluginv1.WatchSyncMediaType, ids simklIDs) bool {
	for _, key := range historyIDMatchKeys(ids) {
		if _, ok := idx[kind.String()+":"+key]; ok {
			return true
		}
	}
	return false
}
