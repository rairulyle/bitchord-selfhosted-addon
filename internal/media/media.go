package media

import (
	"context"
	"errors"
	"net/http"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrUnauthorized = errors.New("unauthorized")
)

// Track is what every backend produces. FileRef and ArtRef are opaque to
// everyone but the adapter that made them. An empty FileRef marks a track
// that cannot be played, such as one whose file is missing on the server.
type Track struct {
	ID          string
	Title       string
	Artist      string
	AlbumArtist string
	Album       string
	DurationSec int
	Codec       string
	Container   string
	BitrateKbps int
	SampleRate  int
	BitDepth    int
	FileRef     string
	ArtRef      string
}

func (t Track) Playable() bool { return t.ID != "" && t.FileRef != "" }

type Backend interface {
	Name() string
	AllTracks(ctx context.Context, library string) ([]Track, error)
	Track(ctx context.Context, id string) (Track, error)
	OpenFile(ctx context.Context, track Track, method string, header http.Header) (*http.Response, error)
	OpenArt(ctx context.Context, track Track) (*http.Response, error)
}

func AudioFormat(codec, container string) string {
	if codec == "pcm" && (container == "wav" || container == "aiff") {
		return container
	}
	if codec == "" {
		return container
	}
	return codec
}

func Lossless(format string) bool {
	switch format {
	case "flac", "alac", "wav", "aiff":
		return true
	}
	return false
}
