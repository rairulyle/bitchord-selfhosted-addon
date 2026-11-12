package server

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultArtMaxItems = 1000
	defaultArtMaxBytes = 32 * 1024 * 1024 // 32MB default
	maxCachedItemSize  = 5 * 1024 * 1024  // 5MB max single item
)

type artItem struct {
	key          string
	body         []byte
	contentType  string
	etag         string
	lastModified string
	size         int64
}

// ArtCache is a thread-safe in-memory LRU cache for artwork images.
type ArtCache struct {
	mu       sync.Mutex
	items    map[string]*list.Element
	evict    *list.List
	maxItems int
	maxBytes int64
	curBytes int64
}

func NewArtCache(maxItems int, maxBytes int64) *ArtCache {
	if maxItems <= 0 {
		maxItems = defaultArtMaxItems
	}
	if maxBytes <= 0 {
		maxBytes = defaultArtMaxBytes
	}
	return &ArtCache{
		items:    make(map[string]*list.Element),
		evict:    list.New(),
		maxItems: maxItems,
		maxBytes: maxBytes,
	}
}

func (c *ArtCache) Get(key string) (*artItem, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	elem, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.evict.MoveToFront(elem)
	return elem.Value.(*artItem), true
}

func (c *ArtCache) Put(key string, body []byte, contentType, etag, lastModified string) *artItem {
	if c == nil {
		return nil
	}
	size := int64(len(body))
	if size > c.maxBytes || size > maxCachedItemSize {
		return nil
	}

	if etag == "" {
		hash := sha256.Sum256(body)
		etag = `"` + hex.EncodeToString(hash[:16]) + `"`
	}

	item := &artItem{
		key:          key,
		body:         body,
		contentType:  contentType,
		etag:         etag,
		lastModified: lastModified,
		size:         size,
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if elem, ok := c.items[key]; ok {
		c.evict.MoveToFront(elem)
		oldItem := elem.Value.(*artItem)
		c.curBytes -= oldItem.size
		elem.Value = item
		c.curBytes += size
		c.prune()
		return item
	}

	elem := c.evict.PushFront(item)
	c.items[key] = elem
	c.curBytes += size
	c.prune()

	return item
}

func (c *ArtCache) prune() {
	for len(c.items) > c.maxItems || (c.curBytes > c.maxBytes && len(c.items) > 0) {
		elem := c.evict.Back()
		if elem == nil {
			break
		}
		c.evict.Remove(elem)
		oldItem := elem.Value.(*artItem)
		delete(c.items, oldItem.key)
		c.curBytes -= oldItem.size
	}
}

func (c *ArtCache) Clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = make(map[string]*list.Element)
	c.evict.Init()
	c.curBytes = 0
}

func (c *ArtCache) Stats() (items int, bytes int64) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.items), c.curBytes
}

func serveCachedArt(w http.ResponseWriter, r *http.Request, item *artItem) {
	if match := r.Header.Get("If-None-Match"); match != "" {
		if match == item.etag || match == "*" || strings.Contains(match, item.etag) {
			w.Header().Set("ETag", item.etag)
			w.Header().Set("Cache-Control", "public, max-age=86400")
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", item.contentType)
	w.Header().Set("Content-Length", strconv.FormatInt(item.size, 10))
	w.Header().Set("ETag", item.etag)
	if item.lastModified != "" {
		w.Header().Set("Last-Modified", item.lastModified)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		w.Write(item.body)
	}
}
