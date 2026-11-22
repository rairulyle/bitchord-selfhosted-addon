package setup

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type staticAsset struct {
	content     []byte
	gzipped     []byte
	version     string
	etag        string
	gzipETag    string
	contentType string
}

func newStaticServer(files fs.FS) http.Handler {
	assets := make(map[string]staticAsset)

	_ = fs.WalkDir(files, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		raw, err := fs.ReadFile(files, p)
		if err != nil {
			return nil
		}

		hash := sha256.Sum256(raw)

		var gz bytes.Buffer
		gw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		_, _ = gw.Write(raw)
		_ = gw.Close()

		ext := filepath.Ext(p)
		ct := mime.TypeByExtension(ext)
		if ct == "" {
			switch ext {
			case ".css":
				ct = "text/css; charset=utf-8"
			case ".js":
				ct = "application/javascript; charset=utf-8"
			case ".svg":
				ct = "image/svg+xml"
			default:
				ct = "application/octet-stream"
			}
		}

		cleanPath := path.Clean("/" + strings.TrimPrefix(strings.TrimPrefix(p, "static/"), "static"))
		assets[cleanPath] = staticAsset{
			content:     raw,
			gzipped:     gz.Bytes(),
			version:     fmt.Sprintf("%x", hash[:8]),
			etag:        fmt.Sprintf(`"%x"`, hash[:16]),
			gzipETag:    fmt.Sprintf(`"%x-gzip"`, hash[:16]),
			contentType: ct,
		}
		return nil
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		securityHeaders(w)
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		asset, ok := assets[path.Clean(r.URL.Path)]
		if !ok {
			http.NotFound(w, r)
			return
		}

		body, etag := asset.content, asset.etag
		serveGzip := acceptsGzip(r.Header.Get("Accept-Encoding")) && len(asset.gzipped) < len(asset.content)
		if serveGzip {
			body, etag = asset.gzipped, asset.gzipETag
		}

		w.Header().Set("ETag", etag)
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Type", asset.contentType)
		if v := r.URL.Query().Get("v"); v != "" && v == asset.version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
		}

		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		if serveGzip {
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

// acceptsGzip reads Accept-Encoding by its q-values, so "gzip;q=0" refuses
// gzip and "*" accepts it unless gzip is named on its own.
func acceptsGzip(header string) bool {
	gzipQ, starQ := -1.0, -1.0
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		q := 1.0
		if name, value, ok := strings.Cut(strings.TrimSpace(params), "="); ok && strings.EqualFold(strings.TrimSpace(name), "q") {
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil {
				continue
			}
			q = parsed
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			gzipQ = max(gzipQ, q)
		case "*":
			starQ = max(starQ, q)
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

func etagMatches(header, etag string) bool {
	for _, item := range strings.Split(header, ",") {
		candidate := strings.TrimPrefix(strings.TrimSpace(item), "W/")
		if candidate == etag || candidate == "*" {
			return true
		}
	}
	return false
}
