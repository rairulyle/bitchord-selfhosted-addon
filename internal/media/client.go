package media

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// StatusError is an upstream status GetJSON did not expect.
type StatusError int

func (e StatusError) Error() string { return "unexpected status " + strconv.Itoa(int(e)) }

type ClientOptions struct {
	BaseURL       string
	Name          string
	TokenVar      string
	Authorize     func(*http.Request)
	HeaderTimeout time.Duration
	CallTimeout   time.Duration
}

type Client struct {
	baseURL     string
	name        string
	tokenVar    string
	authorize   func(*http.Request)
	callTimeout time.Duration
	http        *http.Client
}

func NewClient(o ClientOptions) *Client {
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
		name:        o.Name,
		tokenVar:    o.TokenVar,
		authorize:   o.Authorize,
		callTimeout: o.CallTimeout,
		// No http.Client.Timeout: it would cut a long audio body short.
		http: &http.Client{Transport: transport},
	}
}

// Open answers with the upstream response for any status except 401 and
// 404, which become ErrUnauthorized and ErrNotFound with the body closed.
func (c *Client) Open(ctx context.Context, method, path string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	for key, values := range header {
		req.Header[key] = values
	}
	c.authorize(req)
	// Identity encoding keeps Content-Length and Content-Range true to the file.
	req.Header.Set("Accept-Encoding", "identity")
	res, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.name, err)
	}
	switch res.StatusCode {
	case http.StatusNotFound:
		res.Body.Close()
		return nil, ErrNotFound
	case http.StatusUnauthorized:
		res.Body.Close()
		return nil, fmt.Errorf("%s rejected %s: %w", c.name, c.tokenVar, ErrUnauthorized)
	}
	return res, nil
}

func (c *Client) GetJSON(ctx context.Context, path string, header http.Header, into any) error {
	ctx, cancel := context.WithTimeout(ctx, c.callTimeout)
	defer cancel()
	if header == nil {
		header = http.Header{}
	}
	header.Set("Accept", "application/json")
	res, err := c.Open(ctx, http.MethodGet, path, header)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %w", c.name, StatusError(res.StatusCode))
	}
	if err := json.NewDecoder(res.Body).Decode(into); err != nil {
		return fmt.Errorf("%s: malformed answer: %w", c.name, err)
	}
	return nil
}

// AllPages fetches pages of pageSize until a short page. A page whose first
// key repeats the previous page's first key means the server ignored the
// paging request and answered the whole collection again, so it ends the loop.
func AllPages[T any](pageSize int, key func(T) string, fetch func(start int) ([]T, error)) ([]T, error) {
	var out []T
	var previousFirst string
	havePrevious := false
	for start := 0; ; start += pageSize {
		page, err := fetch(start)
		if err != nil {
			return nil, err
		}
		if havePrevious && len(page) > 0 && key(page[0]) == previousFirst {
			break
		}
		out = append(out, page...)
		if len(page) != pageSize {
			break
		}
		previousFirst = key(page[0])
		havePrevious = true
	}
	return out, nil
}
