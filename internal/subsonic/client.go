package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const (
	apiVersion = "1.16.1"
	clientName = "bitchord-selfhosted-addon"
	pageSize   = 500
)

// Options configures the client. Username and Password come from the stored
// "user:password" credential; Navidrome's Subsonic API has no static API key.
type Options struct {
	BaseURL         string
	Username        string
	Password        string
	ScrobblePercent int
	HeaderTimeout   time.Duration
	CallTimeout     time.Duration
	Log             *slog.Logger
	OnActivity      func(kind, message string)
}

type Client struct {
	http            *media.Client
	username        string
	password        string
	scrobblePercent int
	tracker         *playbackTracker
	log             *slog.Logger
	onActivity      func(kind, message string)
}

// SplitCredential splits the stored credential "user:password"; the first
// colon separates the two so passwords may contain colons of their own.
func SplitCredential(credential string) (username, password string) {
	username, password, _ = strings.Cut(credential, ":")
	return username, password
}

// JoinCredential builds the stored credential from a username and password.
func JoinCredential(username, password string) string {
	return username + ":" + password
}

func New(o Options) *Client {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	percent := o.ScrobblePercent
	if percent == 0 {
		percent = 50
	}
	return &Client{
		http: media.NewClient(media.ClientOptions{
			BaseURL:       strings.TrimRight(o.BaseURL, "/"),
			Name:          "navidrome",
			TokenVar:      "the Navidrome username and password",
			HeaderTimeout: o.HeaderTimeout,
			CallTimeout:   o.CallTimeout,
		}),
		username:        o.Username,
		password:        o.Password,
		scrobblePercent: percent,
		tracker:         &playbackTracker{},
		log:             o.Log,
		onActivity:      o.OnActivity,
	}
}

func (c *Client) SetOnActivity(fn func(kind, message string)) {
	c.onActivity = fn
}

func (c *Client) Name() string { return "Navidrome" }

// authQuery adds Subsonic token authentication: a random salt plus
// md5(password+salt). The password itself never travels on the wire.
func (c *Client) authQuery(q url.Values) url.Values {
	if q == nil {
		q = url.Values{}
	}
	salt := randomSalt()
	sum := md5.Sum([]byte(c.password + salt))
	q.Set("u", c.username)
	q.Set("t", hex.EncodeToString(sum[:]))
	q.Set("s", salt)
	q.Set("v", apiVersion)
	q.Set("c", clientName)
	q.Set("f", "json")
	return q
}

func randomSalt() string {
	raw := make([]byte, 8)
	_, _ = rand.Read(raw)
	return hex.EncodeToString(raw)
}

// subsonicResponse is the outer envelope every answer shares. The inner body
// is kept raw so it can be decoded into each method's own struct.
type subsonicResponse struct {
	Response json.RawMessage `json:"subsonic-response"`
}

type statusHeader struct {
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Error   *apiError `json:"error"`
}

// call performs a get<method> request, checks the Subsonic status, and decodes
// the inner response body into into.
func (c *Client) call(ctx context.Context, method string, params url.Values, into any) error {
	q := c.authQuery(params)
	path := "/rest/" + method + ".view?" + q.Encode()
	var env subsonicResponse
	if err := c.http.GetJSON(ctx, path, nil, &env); err != nil {
		return err
	}
	var header statusHeader
	if err := json.Unmarshal(env.Response, &header); err != nil {
		return fmt.Errorf("navidrome: malformed answer: %w", err)
	}
	if header.Status != "" && header.Status != "ok" {
		if header.Error != nil {
			return navidromeError(header.Error)
		}
		return fmt.Errorf("navidrome: response status %q", header.Status)
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(env.Response, into); err != nil {
		return fmt.Errorf("navidrome: malformed answer: %w", err)
	}
	return nil
}

func navidromeError(e *apiError) error {
	switch e.Code {
	case 40, 41, 10, 50: // wrong credentials, token auth not supported, missing param, unauthorized
		return fmt.Errorf("%w: %s", media.ErrUnauthorized, e.Message)
	case 70: // requested data not found
		return fmt.Errorf("%s: %w", e.Message, media.ErrNotFound)
	}
	return fmt.Errorf("navidrome: %s (code %d)", e.Message, e.Code)
}

func (c *Client) Version(ctx context.Context) (string, error) {
	var answer struct {
		Version string `json:"version"`
	}
	if err := c.call(ctx, "ping", nil, &answer); err != nil {
		return "", err
	}
	return answer.Version, nil
}

func (c *Client) Libraries(ctx context.Context) ([]media.Library, error) {
	var answer struct {
		MusicFolders struct {
			MusicFolder MusicFolderList `json:"musicFolder"`
		} `json:"musicFolders"`
	}
	if err := c.call(ctx, "getMusicFolders", nil, &answer); err != nil {
		return nil, err
	}
	folders := answer.MusicFolders.MusicFolder
	out := make([]media.Library, 0, len(folders))
	for _, folder := range folders {
		out = append(out, media.Library{ID: folder.ID, Name: folder.Name})
	}
	// A single default folder may be omitted; present one so setup lists it.
	if len(out) == 0 {
		out = append(out, media.Library{ID: "", Name: "Music"})
	}
	return out, nil
}

// AllTracks retrieves all tracks from Navidrome. It first attempts to use
// OpenSubsonic's search3 endpoint with an empty query, which Navidrome optimizes
// for fast flat-library synchronization. If search3 is unsupported or returns
// no tracks, it falls back to walking albums via getAlbumList2 with concurrent
// getAlbum workers.
func (c *Client) AllTracks(ctx context.Context, folderFilter string) ([]media.Track, error) {
	tracks, err := c.allTracksSearch3(ctx, folderFilter)
	if err == nil && len(tracks) > 0 {
		return tracks, nil
	}
	if err != nil && !errors.Is(err, media.ErrNotFound) {
		c.log.Debug("navidrome search3 unavailable, falling back to album walk", "error", err)
	}
	return c.allTracksViaAlbums(ctx, folderFilter)
}

// allTracksSearch3 retrieves tracks in pages of 500 using the OpenSubsonic
// empty-query search3 extension.
func (c *Client) allTracksSearch3(ctx context.Context, folderFilter string) ([]media.Track, error) {
	folderID := c.resolveFolderID(ctx, folderFilter)
	var out []media.Track
	seen := make(map[string]bool)
	offset := 0
	for {
		params := url.Values{
			"query":       {""},
			"songCount":   {strconv.Itoa(pageSize)},
			"songOffset":  {strconv.Itoa(offset)},
			"artistCount": {"0"},
			"albumCount":  {"0"},
		}
		if folderID != "" {
			params.Set("musicFolderId", folderID)
		}
		var answer struct {
			SearchResult3 struct {
				Song ChildList `json:"song"`
			} `json:"searchResult3"`
		}
		if err := c.call(ctx, "search3", params, &answer); err != nil {
			return nil, err
		}
		page := answer.SearchResult3.Song
		if len(page) == 0 {
			break
		}
		for _, child := range page {
			if !seen[child.ID] {
				seen[child.ID] = true
				out = append(out, child.convert())
			}
		}
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}
	return out, nil
}

// allTracksViaAlbums walks getAlbumList2 alphabetically, then fetches each
// album's songs via getAlbum with bounded concurrency.
func (c *Client) allTracksViaAlbums(ctx context.Context, folderFilter string) ([]media.Track, error) {
	albums, err := c.allAlbums(ctx, folderFilter)
	if err != nil {
		return nil, err
	}
	if len(albums) == 0 {
		return nil, nil
	}

	type albumResult struct {
		index int
		songs []Child
		err   error
	}

	concurrency := 6
	if len(albums) < concurrency {
		concurrency = len(albums)
	}

	jobs := make(chan int, len(albums))
	results := make(chan albumResult, len(albums))

	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				songs, err := c.albumSongs(ctx, albums[idx].ID)
				results <- albumResult{index: idx, songs: songs, err: err}
			}
		}()
	}

	for i := range albums {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(results)

	ordered := make([][]Child, len(albums))
	for res := range results {
		if res.err != nil {
			return nil, res.err
		}
		ordered[res.index] = res.songs
	}

	var out []media.Track
	seen := make(map[string]bool)
	for _, songList := range ordered {
		for _, song := range songList {
			if !seen[song.ID] {
				seen[song.ID] = true
				out = append(out, song.convert())
			}
		}
	}
	return out, nil
}

func (c *Client) allAlbums(ctx context.Context, folderFilter string) ([]AlbumID3, error) {
	folderID := c.resolveFolderID(ctx, folderFilter)
	var out []AlbumID3
	offset := 0
	for {
		params := url.Values{
			"type":   {"alphabeticalByArtist"},
			"size":   {strconv.Itoa(pageSize)},
			"offset": {strconv.Itoa(offset)},
		}
		if folderID != "" {
			params.Set("musicFolderId", folderID)
		}
		var answer struct {
			AlbumList2 struct {
				Album AlbumList `json:"album"`
			} `json:"albumList2"`
		}
		if err := c.call(ctx, "getAlbumList2", params, &answer); err != nil {
			return nil, err
		}
		page := answer.AlbumList2.Album
		out = append(out, page...)
		if len(page) < pageSize {
			break
		}
		offset += pageSize
	}
	return out, nil
}

func (c *Client) albumSongs(ctx context.Context, albumID string) ([]Child, error) {
	var answer struct {
		Album struct {
			Song ChildList `json:"song"`
		} `json:"album"`
	}
	if err := c.call(ctx, "getAlbum", url.Values{"id": {albumID}}, &answer); err != nil {
		return nil, err
	}
	return answer.Album.Song, nil
}

// resolveFolderID turns a library filter (id or name) into a musicFolderId, or
// "" for the whole collection.
func (c *Client) resolveFolderID(ctx context.Context, filter string) string {
	if filter == "" {
		return ""
	}
	if folders, err := c.Libraries(ctx); err == nil {
		for _, folder := range folders {
			if folder.ID == filter || folder.Name == filter {
				return folder.ID
			}
		}
	}
	return filter // best effort: assume the filter is already an id
}

// Track fetches one song by id via getSong.
func (c *Client) Track(ctx context.Context, id string) (media.Track, error) {
	var answer struct {
		Song Child `json:"song"`
	}
	if err := c.call(ctx, "getSong", url.Values{"id": {id}}, &answer); err != nil {
		if isNotFound(err) || isUnauthorized(err) {
			return media.Track{}, media.ErrNotFound
		}
		return media.Track{}, err
	}
	if answer.Song.ID == "" {
		return media.Track{}, media.ErrNotFound
	}
	return answer.Song.convert(), nil
}

func isNotFound(err error) bool     { return errors.Is(err, media.ErrNotFound) }
func isUnauthorized(err error) bool { return errors.Is(err, media.ErrUnauthorized) }

func (c *Client) OpenFile(ctx context.Context, track media.Track, method string, header http.Header) (*http.Response, error) {
	res, err := c.http.Open(ctx, method, c.authedPath(track.FileRef), header)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && res.StatusCode >= 200 && res.StatusCode < 300 {
		rng := ""
		if header != nil {
			rng = header.Get("Range")
		}
		// If playback is starting from the beginning (no Range or bytes=0-)
		if rng == "" || strings.HasPrefix(rng, "bytes=0-") {
			c.startPlayback(track)
			res.Body = &scrobbleBody{
				ReadCloser: res.Body,
				client:     c,
				trackID:    track.ID,
				total:      res.ContentLength,
			}
		}
	}
	return res, nil
}

type playbackTracker struct {
	mu          sync.Mutex
	trackID     string
	startedAt   time.Time
	durationSec int
	timer       *time.Timer
	scrobbled   bool
}

func (c *Client) startPlayback(track media.Track) {
	trackLabel := track.Title
	if track.Artist != "" {
		trackLabel = track.Title + " — " + track.Artist
	}

	if c.scrobblePercent <= 0 {
		// Scrobbling disabled, only report "Now Playing"
		go func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.Scrobble(sctx, track.ID, false); err == nil && c.onActivity != nil {
				c.onActivity("now_playing", "Now playing: "+trackLabel)
			}
		}()
		return
	}

	c.tracker.mu.Lock()
	defer c.tracker.mu.Unlock()

	// If the client repeats requests for the same track within 5 seconds
	// (e.g. metadata probe range request followed by the actual playback stream),
	// leave the existing playback timer running so it does not reset.
	if c.tracker.trackID == track.ID && time.Since(c.tracker.startedAt) < 5*time.Second {
		return
	}

	// Cancel any pending scrobble timer for a previous track.
	if c.tracker.timer != nil {
		c.tracker.timer.Stop()
	}

	c.tracker.trackID = track.ID
	c.tracker.startedAt = time.Now()
	c.tracker.durationSec = track.DurationSec
	c.tracker.scrobbled = false

	// Report "Now Playing" immediately to Navidrome
	go func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Scrobble(sctx, track.ID, false); err != nil {
			c.log.Debug("navidrome now playing report failed", "id", track.ID, "error", err)
		} else if c.onActivity != nil {
			c.onActivity("now_playing", "Now playing: "+trackLabel)
		}
	}()

	// Calculate needed listen duration in seconds: (duration * percent) / 100
	dur := track.DurationSec
	if dur <= 0 {
		dur = 180 // default 3 min fallback if track metadata omitted duration
	}
	neededSec := float64(dur) * float64(c.scrobblePercent) / 100.0
	if neededSec > 240 {
		neededSec = 240 // Subsonic / Last.fm standard max 4 minutes
	}
	if neededSec < 15 {
		neededSec = 15 // minimum 15s to prevent immediate scrobble on very short tracks
	}

	trackID := track.ID
	c.tracker.timer = time.AfterFunc(time.Duration(neededSec)*time.Second, func() {
		c.tracker.mu.Lock()
		defer c.tracker.mu.Unlock()
		if c.tracker.trackID == trackID && !c.tracker.scrobbled {
			c.tracker.scrobbled = true
			go func() {
				sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if err := c.Scrobble(sctx, trackID, true); err != nil {
					c.log.Debug("navidrome scrobble report failed", "id", trackID, "error", err)
				} else if c.onActivity != nil {
					c.onActivity("scrobble", fmt.Sprintf("Scrobbled: %s (%d%% played)", trackLabel, c.scrobblePercent))
				}
			}()
		}
	})
}

type scrobbleBody struct {
	io.ReadCloser
	client  *Client
	trackID string
	total   int64
	read    int64
	hitEOF  bool
	mu      sync.Mutex
}

func (s *scrobbleBody) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.mu.Lock()
	s.read += int64(n)
	if err == io.EOF {
		s.hitEOF = true
	}
	s.mu.Unlock()
	return n, err
}

func (s *scrobbleBody) Close() error {
	s.mu.Lock()
	read := s.read
	total := s.total
	hitEOF := s.hitEOF
	s.mu.Unlock()

	// Cancel the scrobble timer if playback was aborted early.
	if total > 0 && read*2 < total && !hitEOF {
		s.client.tracker.mu.Lock()
		if s.client.tracker.trackID == s.trackID && !s.client.tracker.scrobbled {
			if s.client.tracker.timer != nil {
				s.client.tracker.timer.Stop()
			}
		}
		s.client.tracker.mu.Unlock()
	}
	return s.ReadCloser.Close()
}

// Scrobble reports playback progress to Navidrome. When submission is false,
// Navidrome registers it as "now playing". When submission is true, Navidrome
// records the completed play and forwards it to external scrobblers (e.g. Last.fm).
func (c *Client) Scrobble(ctx context.Context, id string, submission bool) error {
	params := url.Values{
		"id":         {id},
		"submission": {strconv.FormatBool(submission)},
	}
	return c.call(ctx, "scrobble", params, nil)
}

func (c *Client) OpenArt(ctx context.Context, track media.Track) (*http.Response, error) {
	return c.http.Open(ctx, http.MethodGet, c.authedPath(track.ArtRef), nil)
}

// authedPath appends the Subsonic auth query to a stored ref path.
func (c *Client) authedPath(ref string) string {
	if ref == "" {
		return ""
	}
	base, rawQuery, _ := strings.Cut(ref, "?")
	query, _ := url.ParseQuery(rawQuery)
	return base + "?" + c.authQuery(query).Encode()
}
