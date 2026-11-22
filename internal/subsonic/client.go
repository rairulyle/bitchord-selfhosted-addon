package subsonic

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
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

// DefaultScrobblePercent is the share of a track played before a scrobble
// when a server stores no percent of its own.
const DefaultScrobblePercent = 50

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
	pageSize        int
	plays           *plays
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
		percent = DefaultScrobblePercent
	}
	c := &Client{
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
		pageSize:        pageSize,
		log:             o.Log,
		onActivity:      o.OnActivity,
	}
	c.plays = newPlays(c)
	return c
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
		return redact(err)
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
	case 40, 41, 50: // wrong credentials, token auth not supported, user not authorized
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

// allTracksSearch3 retrieves tracks in pages using the OpenSubsonic
// empty-query search3 extension.
func (c *Client) allTracksSearch3(ctx context.Context, folderFilter string) ([]media.Track, error) {
	folderID := c.resolveFolderID(ctx, folderFilter)
	songs, err := media.AllPages(c.pageSize, func(song Child) string { return song.ID }, func(offset int) ([]Child, error) {
		params := url.Values{
			"query":       {""},
			"songCount":   {strconv.Itoa(c.pageSize)},
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
		return answer.SearchResult3.Song, nil
	})
	if err != nil {
		return nil, err
	}
	return convertUnique(songs), nil
}

const albumWorkers = 6

// allTracksViaAlbums walks getAlbumList2 alphabetically, then fetches each
// album's songs via getAlbum with bounded concurrency. The first failure
// cancels the walk.
func (c *Client) allTracksViaAlbums(ctx context.Context, folderFilter string) ([]media.Track, error) {
	albums, err := c.allAlbums(ctx, folderFilter)
	if err != nil || len(albums) == 0 {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	jobs := make(chan int)
	songs := make([][]Child, len(albums))
	var wg sync.WaitGroup
	for range min(albumWorkers, len(albums)) {
		wg.Go(func() {
			for i := range jobs {
				found, err := c.albumSongs(ctx, albums[i].ID)
				if err != nil {
					cancel(err)
					return
				}
				songs[i] = found
			}
		})
	}
feed:
	for i := range albums {
		select {
		case jobs <- i:
		case <-ctx.Done():
			break feed
		}
	}
	close(jobs)
	wg.Wait()
	if err := context.Cause(ctx); err != nil {
		return nil, err
	}
	return convertUnique(slices.Concat(songs...)), nil
}

func convertUnique(songs []Child) []media.Track {
	out := make([]media.Track, 0, len(songs))
	seen := make(map[string]bool, len(songs))
	for _, song := range songs {
		if !seen[song.ID] {
			seen[song.ID] = true
			out = append(out, song.convert())
		}
	}
	return out
}

func (c *Client) allAlbums(ctx context.Context, folderFilter string) ([]AlbumID3, error) {
	folderID := c.resolveFolderID(ctx, folderFilter)
	return media.AllPages(c.pageSize, func(album AlbumID3) string { return album.ID }, func(offset int) ([]AlbumID3, error) {
		params := url.Values{
			"type":   {"alphabeticalByArtist"},
			"size":   {strconv.Itoa(c.pageSize)},
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
		return answer.AlbumList2.Album, nil
	})
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
		if isNotFound(err) {
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
	res, err := c.open(ctx, method, track.FileRef, header)
	if err != nil {
		return nil, err
	}
	if method == http.MethodGet && res.StatusCode >= 200 && res.StatusCode < 300 {
		c.plays.played(track, header.Get("Range"))
	}
	return res, nil
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
	return c.open(ctx, http.MethodGet, track.ArtRef, nil)
}

// open fetches a media endpoint. Subsonic servers answer a failed stream or
// cover art request with HTTP 200 and an error envelope, which must not be
// passed on as audio or cached as artwork.
func (c *Client) open(ctx context.Context, method, ref string, header http.Header) (*http.Response, error) {
	res, err := c.http.Open(ctx, method, c.authedPath(ref), header)
	if err != nil {
		return nil, redact(err)
	}
	mediaType, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
	switch mediaType {
	case "application/json", "text/xml", "application/xml":
	default:
		return res, nil
	}
	defer res.Body.Close()
	return nil, envelopeError(io.LimitReader(res.Body, maxEnvelopeBytes), mediaType)
}

const maxEnvelopeBytes = 64 << 10

func envelopeError(body io.Reader, mediaType string) error {
	var header statusHeader
	if mediaType == "application/json" {
		var env subsonicResponse
		if json.NewDecoder(body).Decode(&env) == nil {
			_ = json.Unmarshal(env.Response, &header)
		}
	} else {
		var env struct {
			Status string `xml:"status,attr"`
			Error  *struct {
				Code    int    `xml:"code,attr"`
				Message string `xml:"message,attr"`
			} `xml:"error"`
		}
		if xml.NewDecoder(body).Decode(&env) == nil {
			header.Status = env.Status
			if env.Error != nil {
				header.Error = &apiError{Code: env.Error.Code, Message: env.Error.Message}
			}
		}
	}
	if header.Error != nil {
		return navidromeError(header.Error)
	}
	return fmt.Errorf("navidrome: answered %s instead of media", mediaType)
}

// redact drops the query from a transport error's URL: it carries the
// username and the salted password token, and the error reaches logs, the
// setup page and webhook answers.
func redact(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	bare, _, _ := strings.Cut(urlErr.URL, "?")
	return fmt.Errorf("navidrome: %w", &url.Error{Op: urlErr.Op, URL: bare, Err: urlErr.Err})
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
