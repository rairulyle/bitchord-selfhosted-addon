package plex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var (
	ErrNotFound     = errors.New("plex: not found")
	ErrUnauthorized = errors.New("plex: token rejected")
)

type Options struct {
	BaseURL       string
	Token         string
	PageSize      int
	HeaderTimeout time.Duration
	CallTimeout   time.Duration
}

type Client struct {
	baseURL     string
	token       string
	pageSize    int
	callTimeout time.Duration
	http        *http.Client
}

func New(o Options) *Client {
	if o.PageSize <= 0 {
		o.PageSize = 1000
	}
	if o.HeaderTimeout <= 0 {
		o.HeaderTimeout = 10 * time.Second
	}
	if o.CallTimeout <= 0 {
		o.CallTimeout = 30 * time.Second
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = o.HeaderTimeout
	return &Client{
		baseURL:     o.BaseURL,
		token:       o.Token,
		pageSize:    o.PageSize,
		callTimeout: o.CallTimeout,
		// No http.Client.Timeout: it would cut a long audio body short.
		http: &http.Client{Transport: transport},
	}
}

func (c *Client) Open(ctx context.Context, method, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		req.Header[key] = values
	}
	req.Header.Set("X-Plex-Token", c.token)
	// Identity encoding keeps Content-Length and Content-Range true to the file.
	req.Header.Set("Accept-Encoding", "identity")
	return c.http.Do(req)
}

func (c *Client) getJSON(ctx context.Context, path string, header http.Header, into any) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Accept", "application/json")
	res, err := c.Open(ctx, http.MethodGet, path, header)
	if err != nil {
		return fmt.Errorf("plex: %w", err)
	}
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case res.StatusCode == http.StatusUnauthorized:
		return ErrUnauthorized
	case res.StatusCode != http.StatusOK:
		return fmt.Errorf("plex: unexpected status %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		return fmt.Errorf("plex: malformed answer: %w", err)
	}
	return nil
}

func (c *Client) MusicSections(ctx context.Context, filter string) ([]Section, error) {
	var answer struct {
		MediaContainer struct {
			Directory []Section `json:"Directory"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, "/library/sections", nil, &answer); err != nil {
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

func (c *Client) AllTracks(ctx context.Context, filter string) ([]Track, error) {
	sections, err := c.MusicSections(ctx, filter)
	if err != nil {
		return nil, err
	}
	var out []Track
	for _, section := range sections {
		var previousFirst string
		havePrevious := false
		for start := 0; ; start += c.pageSize {
			page, err := c.trackPage(ctx, section.Key, start)
			if err != nil {
				return nil, err
			}
			if havePrevious && len(page) > 0 && page[0].RatingKey == previousFirst {
				break
			}
			out = append(out, page...)
			if len(page) != c.pageSize {
				break
			}
			previousFirst = page[0].RatingKey
			havePrevious = true
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
	if err := c.getJSON(ctx, path, header, &answer); err != nil {
		return nil, err
	}
	return answer.MediaContainer.Metadata, nil
}

func (c *Client) Track(ctx context.Context, id string) (Track, error) {
	var answer struct {
		MediaContainer struct {
			Metadata []Track `json:"Metadata"`
		} `json:"MediaContainer"`
	}
	if err := c.getJSON(ctx, "/library/metadata/"+url.PathEscape(id), nil, &answer); err != nil {
		return Track{}, err
	}
	if len(answer.MediaContainer.Metadata) == 0 {
		return Track{}, ErrNotFound
	}
	return answer.MediaContainer.Metadata[0], nil
}

func ArtPath(thumb string) string {
	return "/photo/:/transcode?width=600&height=600&minSize=1&upscale=1&url=" + url.QueryEscape(thumb)
}
