package subsonic

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

// StringID unmarshals from either a JSON string (e.g. "0", "abc") or a JSON
// number (e.g. 0, 1), converting it to a string. This accommodates Subsonic
// servers where fields like musicFolder.id are integers in the XSD specification
// while others are strings.
type StringID string

func (s *StringID) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = ""
		return nil
	}
	if trimmed[0] == '"' {
		var str string
		if err := json.Unmarshal(trimmed, &str); err != nil {
			return err
		}
		*s = StringID(str)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(trimmed, &n); err != nil {
		return err
	}
	*s = StringID(n.String())
	return nil
}

func (s StringID) String() string {
	return string(s)
}

// The Subsonic API wraps every answer in {"subsonic-response": {...}}. The
// status field is "ok" or "failed"; a failed answer carries an error object.
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// MusicFolder is one library, as getMusicFolders lists them. The Subsonic
// specification defines its id as an integer, though some servers or proxies
// pass strings; UnmarshalJSON accepts both and stores it as a string.
type MusicFolder struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (m *MusicFolder) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID   StringID `json:"id"`
		Name string   `json:"name"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	m.ID = string(raw.ID)
	m.Name = raw.Name
	return nil
}

// MusicFolderList unmarshals from either a JSON array or a single JSON object,
// guarding against XML-to-JSON gateways that emit a bare object for single items.
type MusicFolderList []MusicFolder

func (l *MusicFolderList) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []MusicFolder
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*l = list
		return nil
	}
	var single MusicFolder
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	*l = []MusicFolder{single}
	return nil
}

// AlbumID3 is an album from getAlbumList2.
type AlbumID3 struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Artist    string `json:"artist"`
	ArtistID  string `json:"artistId"`
	CoverArt  string `json:"coverArt"`
	SongCount int    `json:"songCount"`
}

// AlbumList unmarshals from either a JSON array or a single JSON object.
type AlbumList []AlbumID3

func (l *AlbumList) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []AlbumID3
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*l = list
		return nil
	}
	var single AlbumID3
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	*l = []AlbumID3{single}
	return nil
}

// Child is a song, as getAlbum, getSong, or search3 returns it.
type Child struct {
	ID                 string `json:"id"`
	Parent             string `json:"parent"`
	Title              string `json:"title"`
	Album              string `json:"album"`
	Artist             string `json:"artist"`
	AlbumArtist        string `json:"albumArtist"`
	DisplayAlbumArtist string `json:"displayAlbumArtist"`
	DisplayArtist      string `json:"displayArtist"`
	Track              int    `json:"track"`
	Duration           int    `json:"duration"` // seconds
	BitRate            int    `json:"bitRate"`  // kbps
	Suffix             string `json:"suffix"`
	ContentType        string `json:"contentType"`
	Size               int64  `json:"size"`
	CoverArt           string `json:"coverArt"`
	DiscNumber         int    `json:"discNumber"`
	Type               string `json:"type"`
	IsDir              bool   `json:"isDir"`
	SamplingRate       int    `json:"samplingRate"`
	BitDepth           int    `json:"bitDepth"`
}

// ChildList unmarshals from either a JSON array or a single JSON object.
type ChildList []Child

func (l *ChildList) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*l = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []Child
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		*l = list
		return nil
	}
	var single Child
	if err := json.Unmarshal(trimmed, &single); err != nil {
		return err
	}
	*l = []Child{single}
	return nil
}

// convert turns a Subsonic song into the addon's common Track. Codec comes
// from the file suffix so the client can decide whether it is lossless.
func (c Child) convert() media.Track {
	artist := c.DisplayArtist
	if artist == "" {
		artist = c.Artist
	}
	albumArtist := c.DisplayAlbumArtist
	if albumArtist == "" {
		albumArtist = c.AlbumArtist
	}
	if albumArtist == "" {
		albumArtist = artist
	}
	out := media.Track{
		ID:          c.ID,
		Title:       c.Title,
		Artist:      artist,
		AlbumArtist: albumArtist,
		Album:       c.Album,
		DurationSec: c.Duration,
		BitrateKbps: c.BitRate,
		SampleRate:  c.SamplingRate,
		BitDepth:    c.BitDepth,
		Container:   strings.ToLower(c.Suffix),
		Codec:       media.AudioFormat(codecOf(c.Suffix, c.ContentType), strings.ToLower(c.Suffix)),
	}
	if c.CoverArt != "" {
		out.ArtRef = ArtPath(c.CoverArt)
	}
	// A song with no id or no suffix has no playable file.
	if c.ID != "" && c.Suffix != "" && !c.IsDir {
		out.FileRef = StreamPath(c.ID)
	}
	return out
}

// codecOf maps a file suffix or MIME type to the codec name the rest of the
// addon uses, so Lossless() and the playable-source preference behave as for
// other servers.
func codecOf(suffix, contentType string) string {
	switch strings.ToLower(suffix) {
	case "flac":
		return "flac"
	case "alac":
		return "alac"
	case "m4a":
		// Most .m4a from a server is AAC; only label ALAC when the server says
		// so, since the difference changes whether a track counts as lossless.
		if strings.Contains(strings.ToLower(contentType), "alac") {
			return "alac"
		}
		return "aac"
	case "mp3":
		return "mp3"
	case "aac":
		return "aac"
	case "opus":
		return "opus"
	case "ogg", "oga":
		ct := strings.ToLower(contentType)
		if strings.Contains(ct, "flac") {
			return "flac"
		}
		if strings.Contains(ct, "opus") {
			return "opus"
		}
		return "vorbis"
	case "wav":
		return "pcm"
	case "aiff", "aif":
		return "pcm"
	case "wma":
		return "wma"
	case "wv":
		return "wavpack"
	case "ape":
		return "ape"
	case "dsf", "dff":
		return "dsd"
	case "":
		// Fall back to the MIME subtype, e.g. "audio/flac".
		if i := strings.Index(contentType, "/"); i >= 0 {
			return strings.ToLower(contentType[i+1:])
		}
		return ""
	default:
		return strings.ToLower(suffix)
	}
}

// StreamPath is the Subsonic endpoint that streams the original file, without
// re-encoding, with support for range requests so seeking works.
func StreamPath(id string) string {
	return "/rest/stream.view?id=" + url.QueryEscape(id) + "&format=raw"
}

// ArtPath is the cover-art endpoint, square-cropped to a size BitChord uses.
func ArtPath(id string) string {
	return "/rest/getCoverArt.view?id=" + url.QueryEscape(id) + "&size=600"
}
