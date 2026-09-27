package jellyfin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

const clientName = "bitchord-selfhosted-addon"

type Options struct {
	BaseURL       string
	APIKey        string
	DeviceID      string
	Version       string
	PageSize      int
	HeaderTimeout time.Duration
	CallTimeout   time.Duration
}

type Client struct {
	http     *media.Client
	pageSize int
}

// Only Token is required by the server, and a sign-in request carries none.
// The other fields name the addon in the Jellyfin dashboard.
func Authorization(version, deviceID, key string) string {
	if deviceID == "" {
		deviceID = clientName
	}
	header := fmt.Sprintf(`MediaBrowser Client=%q, Device="server", DeviceId=%q, Version=%q`, clientName, deviceID, version)
	if key == "" {
		return header
	}
	return header + fmt.Sprintf(`, Token=%q`, key)
}

func New(o Options) *Client {
	if o.PageSize <= 0 {
		o.PageSize = 1000
	}
	if o.Version == "" {
		o.Version = "dev"
	}
	authorization := Authorization(o.Version, o.DeviceID, o.APIKey)
	return &Client{
		http: media.NewClient(media.ClientOptions{
			BaseURL:       o.BaseURL,
			Name:          "jellyfin",
			TokenVar:      "the Jellyfin API key",
			Authorize:     func(r *http.Request) { r.Header.Set("Authorization", authorization) },
			HeaderTimeout: o.HeaderTimeout,
			CallTimeout:   o.CallTimeout,
		}),
		pageSize: o.PageSize,
	}
}

func (c *Client) Name() string { return "Jellyfin" }

func (c *Client) Version(ctx context.Context) (string, error) {
	var answer struct {
		Version string `json:"Version"`
	}
	if err := c.http.GetJSON(ctx, "/System/Info", nil, &answer); err != nil {
		return "", err
	}
	return answer.Version, nil
}

func (c *Client) Libraries(ctx context.Context) ([]media.Library, error) {
	folders, err := c.folders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]media.Library, len(folders))
	for i, folder := range folders {
		out[i] = media.Library{ID: folder.ID, Name: folder.Name}
	}
	return out, nil
}

func (c *Client) folders(ctx context.Context) ([]Folder, error) {
	var answer struct {
		Items []Folder `json:"Items"`
	}
	if err := c.http.GetJSON(ctx, "/Library/MediaFolders", nil, &answer); err != nil {
		return nil, err
	}
	var out []Folder
	for _, folder := range answer.Items {
		if strings.EqualFold(folder.CollectionType, "music") {
			out = append(out, folder)
		}
	}
	return out, nil
}

func (c *Client) musicFolders(ctx context.Context, filter string) ([]Folder, error) {
	folders, err := c.folders(ctx)
	if err != nil {
		return nil, err
	}
	var out []Folder
	for _, folder := range folders {
		if filter == "" || filter == folder.ID || filter == folder.Name {
			out = append(out, folder)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jellyfin: no music library matches %q", filter)
	}
	return out, nil
}

func (c *Client) AllTracks(ctx context.Context, filter string) ([]media.Track, error) {
	folders, err := c.musicFolders(ctx, filter)
	if err != nil {
		return nil, err
	}
	var out []media.Track
	for _, folder := range folders {
		items, err := media.AllPages(c.pageSize,
			func(i Item) string { return i.ID },
			func(start int) ([]Item, error) { return c.itemPage(ctx, folder.ID, start) })
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			out = append(out, item.convert())
		}
	}
	return out, nil
}

func (c *Client) items(ctx context.Context, query url.Values) ([]Item, error) {
	query.Set("Fields", "MediaSources")
	var answer struct {
		Items []Item `json:"Items"`
	}
	if err := c.http.GetJSON(ctx, "/Items?"+query.Encode(), nil, &answer); err != nil {
		return nil, err
	}
	return answer.Items, nil
}

func (c *Client) itemPage(ctx context.Context, parentID string, start int) ([]Item, error) {
	return c.items(ctx, url.Values{
		"IncludeItemTypes":       {"Audio"},
		"Recursive":              {"true"},
		"ParentId":               {parentID},
		"SortBy":                 {"SortName"},
		"SortOrder":              {"Ascending"},
		"EnableTotalRecordCount": {"false"},
		"StartIndex":             {strconv.Itoa(start)},
		"Limit":                  {strconv.Itoa(c.pageSize)},
	})
}

// GET /Items/{id} exists from 10.9 on and answers 405 before that, so a single
// item is fetched as a filtered list, which every version serves.
// An id that is not a GUID gets a 400, which is as good as not found.
func (c *Client) Track(ctx context.Context, id string) (media.Track, error) {
	items, err := c.items(ctx, url.Values{"Ids": {id}, "IncludeItemTypes": {"Audio"}})
	var status media.StatusError
	if errors.As(err, &status) && status == http.StatusBadRequest {
		return media.Track{}, media.ErrNotFound
	}
	if err != nil {
		return media.Track{}, err
	}
	if len(items) == 0 {
		return media.Track{}, media.ErrNotFound
	}
	return items[0].convert(), nil
}

func (c *Client) OpenFile(ctx context.Context, track media.Track, method string, header http.Header) (*http.Response, error) {
	return c.http.Open(ctx, method, track.FileRef, header)
}

func (c *Client) OpenArt(ctx context.Context, track media.Track) (*http.Response, error) {
	return c.http.Open(ctx, http.MethodGet, track.ArtRef, nil)
}
