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
	etag        string
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
		etag := fmt.Sprintf(`"%x"`, hash[:16])

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
			etag:        etag,
			contentType: ct,
		}
		return nil
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		assetPath := path.Clean(r.URL.Path)
		asset, ok := assets[assetPath]
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("ETag", asset.etag)
		w.Header().Set("Cache-Control", "no-cache, max-age=0, must-revalidate")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Type", asset.contentType)

		if match := r.Header.Get("If-None-Match"); match != "" {
			for _, item := range strings.Split(match, ",") {
				trimmed := strings.TrimSpace(item)
				if trimmed == asset.etag || trimmed == "*" {
					w.WriteHeader(http.StatusNotModified)
					return
				}
			}
		}

		serveGzip := strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && len(asset.gzipped) < len(asset.content)
		body := asset.content
		if serveGzip {
			w.Header().Set("Content-Encoding", "gzip")
			body = asset.gzipped
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
