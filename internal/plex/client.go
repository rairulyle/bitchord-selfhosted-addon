package plex

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type Options struct {
	BaseURL       string
	Token         string
	PageSize      int
	HeaderTimeout time.Duration
	CallTimeout   time.Duration
	Log           *slog.Logger
}

type Client struct {
	http     *media.Client
	pageSize int
	log      *slog.Logger
}

func New(o Options) *Client {
	if o.PageSize <= 0 {
		o.PageSize = 1000
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	token := o.Token
	return &Client{
		http: media.NewClient(media.ClientOptions{
			BaseURL:       o.BaseURL,
			Name:          "plex",
			TokenVar:      "PLEX_TOKEN",
			Authorize:     func(r *http.Request) { r.Header.Set("X-Plex-Token", token) },
			HeaderTimeout: o.HeaderTimeout,
			CallTimeout:   o.CallTimeout,
		}),
		pageSize: o.PageSize,
		log:      o.Log,
	}
}

func (c *Client) Name() string { return "Plex" }

func (c *Client) musicSections(ctx context.Context, filter string) ([]Section, error) {
	var answer struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.http.GetJSON(ctx, "/library/sections", nil, &answer); err != nil {
		return nil, err
	}
	var out []Section
	for _, section := range answer.MediaContainer.Directory {
		if section.Type != "artist" {
			continue
		}
		if filter == "" || filter == section.Key || filter == section.Title {
			out = append(out, section)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("plex: no music section matches %q", filter)
	}
	return out, nil
}

func (c *Client) AllTracks(ctx context.Context, filter string) ([]media.Track, error) {
	sections, err := c.musicSections(ctx, filter)
	if err != nil {
		return nil, err
	}
	var out []media.Track
	for _, section := range sections {
		items, err := media.AllPages(c.pageSize,
			func(t Track) string { return t.RatingKey },
			func(start int) ([]Track, error) { return c.trackPage(ctx, section.Key, start) })
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			out = append(out, item.convert())
		}
	}
	return out, nil
}

func (c *Client) trackPage(ctx context.Context, sectionKey string, start int) ([]Track, error) {
	var answer struct {
		MediaContainer struct {
			Metadata []Track `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	header := http.Header{}
	header.Set("X-Plex-Container-Start", strconv.Itoa(start))
	header.Set("X-Plex-Container-Size", strconv.Itoa(c.pageSize))
	path := "/library/sections/" + url.PathEscape(sectionKey) + "/all?type=10"
	if err := c.http.GetJSON(ctx, path, header, &answer); err != nil {
		return nil, err
	}
	return answer.MediaContainer.Metadata, nil
}

// An id Plex cannot parse as a rating key may get a 400, which is as good as
// not found.
func (c *Client) Track(ctx context.Context, id string) (media.Track, error) {
	var answer struct {
		MediaContainer struct {
			Metadata []Track `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	err := c.http.GetJSON(ctx, "/library/metadata/"+url.PathEscape(id), nil, &answer)
	var status media.StatusError
	if errors.As(err, &status) && status == http.StatusBadRequest {
		return media.Track{}, media.ErrNotFound
	}
	if err != nil {
		return media.Track{}, err
	}
	if len(answer.MediaContainer.Metadata) == 0 {
		return media.Track{}, media.ErrNotFound
	}
	return answer.MediaContainer.Metadata[0].convert(), nil
}

// Plex answers 500 to a direct-play request for a track it never finished
// analysing (no media bitrate), yet serves the same part as a download.
func (c *Client) OpenFile(ctx context.Context, track media.Track, method string, header http.Header) (*http.Response, error) {
	res, err := c.http.Open(ctx, method, track.FileRef, header)
	if err != nil || res.StatusCode != http.StatusInternalServerError {
		return res, err
	}
	res.Body.Close()
	c.log.Debug("plex refused direct play, retrying as a download", "id", track.ID)
	return c.http.Open(ctx, method, track.FileRef+"?download=1", header)
}

func (c *Client) OpenArt(ctx context.Context, track media.Track) (*http.Response, error) {
	return c.http.Open(ctx, http.MethodGet, ArtPath(track.ArtRef), nil)
}

func ArtPath(thumb string) string {
	return "/photo/:/transcode?width=600&height=600&minSize=1&upscale=1&url=" + url.QueryEscape(thumb)
}
