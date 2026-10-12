package provider

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type pinCodeResponse struct {
	Result          string `json:"result"`
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURL string `json:"verification_url"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type pinStatusResponse struct {
	Result      string `json:"result"`
	Message     string `json:"message"`
	AccessToken string `json:"access_token"`
}

// oauth2DeviceResponse answers POST /oauth2/device (RFC 8628 section 3.2).
type oauth2DeviceResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
}

// oauth2TokenResponse answers POST /oauth2/token for the device and refresh
// grants alike.
type oauth2TokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

type userSettingsResponse struct {
	User struct {
		Name string `json:"name"`
	} `json:"user"`
	Account struct {
		ID int `json:"id"`
	} `json:"account"`
}

type simklActivities struct {
	All     string              `json:"all"`
	Movies  simklActivityBucket `json:"movies"`
	TVShows simklActivityBucket `json:"tv_shows"`
	Anime   simklActivityBucket `json:"anime"`
}

type simklActivityBucket struct {
	All             string `json:"all"`
	Playback        string `json:"playback"`
	Watching        string `json:"watching"`
	Completed       string `json:"completed"`
	RemovedFromList string `json:"removed_from_list"`
	RatedAt         string `json:"rated_at"`
}

type simklAllItemsResponse struct {
	Movies []simklMovieItem `json:"movies"`
	Shows  []simklShowItem  `json:"shows"`
	Anime  []simklShowItem  `json:"anime"`
}

type simklMovieItem struct {
	Status        string     `json:"status"`
	LastWatchedAt *time.Time `json:"last_watched_at"`
	Movie         simklMovie `json:"movie"`
}

type simklShowItem struct {
	Status        string        `json:"status"`
	LastWatchedAt *time.Time    `json:"last_watched_at"`
	Show          simklShow     `json:"show"`
	Seasons       []simklSeason `json:"seasons"`
}

type simklSeason struct {
	Number   int            `json:"number"`
	Episodes []simklEpisode `json:"episodes"`
}

type simklEpisode struct {
	Title      string `json:"title"`
	Season     int    `json:"season"`
	Number     int    `json:"number"`
	Episode    int    `json:"episode"`
	TVDBSeason int    `json:"tvdb_season"`
	TVDBNumber int    `json:"tvdb_number"`
	TVDB       struct {
		Season  int `json:"season"`
		Episode int `json:"episode"`
	} `json:"tvdb"`
	WatchedAt *time.Time `json:"watched_at"`
	IDs       simklIDs   `json:"ids"`
}

type simklPlayback struct {
	ID       int64        `json:"id"`
	Type     string       `json:"type"`
	Progress float64      `json:"progress"`
	PausedAt time.Time    `json:"paused_at"`
	Movie    simklMovie   `json:"movie"`
	Show     simklShow    `json:"show"`
	Anime    simklShow    `json:"anime"`
	Episode  simklEpisode `json:"episode"`
}

type simklMovie struct {
	Title string   `json:"title"`
	Year  int      `json:"year"`
	IDs   simklIDs `json:"ids"`
}

type simklShow struct {
	Title string   `json:"title"`
	Year  int      `json:"year"`
	IDs   simklIDs `json:"ids"`
}

// simklIDs holds the ids Prairie uses. Simkl sends numeric ids as numbers or as
// strings depending on the endpoint, so decoding accepts both.
type simklIDs struct {
	Simkl int    `json:"simkl,omitempty"`
	Slug  string `json:"slug,omitempty"`
	IMDb  string `json:"imdb,omitempty"`
	TMDB  int    `json:"tmdb,omitempty"`
	TVDB  int    `json:"tvdb,omitempty"`
}

func (ids *simklIDs) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	ids.Simkl = intFromJSON(raw["simkl"])
	ids.Slug = stringFromJSON(raw["slug"])
	ids.IMDb = stringFromJSON(raw["imdb"])
	ids.TMDB = intFromJSON(raw["tmdb"])
	ids.TVDB = intFromJSON(raw["tvdb"])
	return nil
}

type simklHistoryPayload struct {
	Movies   []simklHistoryMovie   `json:"movies,omitempty"`
	Shows    []simklHistoryShow    `json:"shows,omitempty"`
	Episodes []simklHistoryEpisode `json:"episodes,omitempty"`
}

type simklHistoryMovie struct {
	Title     string   `json:"title,omitempty"`
	Year      int      `json:"year,omitempty"`
	WatchedAt string   `json:"watched_at,omitempty"`
	IDs       simklIDs `json:"ids"`
}

type simklHistoryShow struct {
	Title   string               `json:"title,omitempty"`
	Year    int                  `json:"year,omitempty"`
	IDs     simklIDs             `json:"ids"`
	Seasons []simklHistorySeason `json:"seasons,omitempty"`
}

type simklHistorySeason struct {
	Number    int                   `json:"number"`
	Episodes  []simklHistoryEpisode `json:"episodes,omitempty"`
	WatchedAt string                `json:"watched_at,omitempty"`
}

type simklHistoryEpisode struct {
	Number    int      `json:"number,omitempty"`
	WatchedAt string   `json:"watched_at,omitempty"`
	IDs       simklIDs `json:"ids"`
}

type simklHistoryResponse struct {
	NotFound struct {
		Movies   []simklHistoryMovie   `json:"movies"`
		Shows    []simklHistoryShow    `json:"shows"`
		Episodes []simklHistoryEpisode `json:"episodes"`
	} `json:"not_found"`
}

type simklListPayload struct {
	Movies []simklListItem `json:"movies,omitempty"`
	Shows  []simklListItem `json:"shows,omitempty"`
}

type simklListItem struct {
	To  string   `json:"to,omitempty"`
	IDs simklIDs `json:"ids"`
}

// movieKey, showKey, and episodeKey produce the provider item keys Prairie stores
// per item. They match the keys of Prairie's former built-in Simkl provider byte
// for byte, so connections migrated from it keep their stored rows.
func movieKey(ids simklIDs) string {
	switch {
	case ids.IMDb != "":
		return "imdb:" + ids.IMDb
	case ids.TMDB > 0:
		return "tmdb:" + strconv.Itoa(ids.TMDB)
	case ids.TVDB > 0:
		return "tvdb:" + strconv.Itoa(ids.TVDB)
	case ids.Simkl > 0:
		return "simkl:" + strconv.Itoa(ids.Simkl)
	default:
		return ""
	}
}

func showKey(ids simklIDs) string {
	switch {
	case ids.TVDB > 0:
		return "tvdb:" + strconv.Itoa(ids.TVDB)
	case ids.TMDB > 0:
		return "tmdb:" + strconv.Itoa(ids.TMDB)
	case ids.IMDb != "":
		return "imdb:" + ids.IMDb
	case ids.Simkl > 0:
		return "simkl:" + strconv.Itoa(ids.Simkl)
	default:
		return ""
	}
}

func episodeKey(showIDs simklIDs, season, episode int, episodeIDs simklIDs) string {
	switch {
	case episodeIDs.TVDB > 0:
		return "tvdb:" + strconv.Itoa(episodeIDs.TVDB)
	case episodeIDs.TMDB > 0:
		return "tmdb:" + strconv.Itoa(episodeIDs.TMDB)
	case episodeIDs.Simkl > 0:
		return "simkl:" + strconv.Itoa(episodeIDs.Simkl)
	case showIDs.TVDB > 0:
		return fmt.Sprintf("show:tvdb:%d:s%d:e%d", showIDs.TVDB, season, episode)
	case showIDs.TMDB > 0:
		return fmt.Sprintf("show:tmdb:%d:s%d:e%d", showIDs.TMDB, season, episode)
	case showIDs.IMDb != "":
		return fmt.Sprintf("show:imdb:%s:s%d:e%d", showIDs.IMDb, season, episode)
	default:
		return ""
	}
}

// episodeNumbers prefers the TVDB numbering Simkl reports for an episode, which
// is how anime episodes line up with Prairie's TVDB-ordered seasons.
func episodeNumbers(episode simklEpisode, seasonFallback int) (int, int) {
	season := episode.TVDB.Season
	number := episode.TVDB.Episode
	if season == 0 {
		season = episode.TVDBSeason
	}
	if number == 0 {
		number = episode.TVDBNumber
	}
	if season == 0 {
		season = episode.Season
	}
	if season == 0 {
		season = seasonFallback
	}
	if number == 0 {
		number = episode.Number
	}
	if number == 0 {
		number = episode.Episode
	}
	return season, number
}

func idsFromLocal(imdbID, tmdbID, tvdbID string) simklIDs {
	return simklIDs{IMDb: imdbID, TMDB: parseInt(tmdbID), TVDB: parseInt(tvdbID)}
}

func idsFromProviderItemKey(key string) simklIDs {
	prefix, value, ok := strings.Cut(key, ":")
	if !ok || value == "" {
		return simklIDs{}
	}
	switch prefix {
	case "imdb":
		return simklIDs{IMDb: value}
	case "tmdb":
		return simklIDs{TMDB: parseInt(value)}
	case "tvdb":
		return simklIDs{TVDB: parseInt(value)}
	case "simkl":
		return simklIDs{Simkl: parseInt(value)}
	default:
		return simklIDs{}
	}
}

func intString(value int) string {
	if value == 0 {
		return ""
	}
	return strconv.Itoa(value)
}

func parseInt(value string) int {
	parsed, _ := strconv.Atoi(value)
	return parsed
}

func intFromJSON(value any) int {
	switch v := value.(type) {
	case float64:
		return int(v)
	case string:
		parsed, _ := strconv.Atoi(v)
		return parsed
	default:
		return 0
	}
}

func stringFromJSON(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
