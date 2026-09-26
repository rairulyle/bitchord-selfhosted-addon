package jellyfin

import (
	"cmp"
	"net/url"
	"strings"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type Folder struct {
	ID             string `json:"Id"`
	Name           string `json:"Name"`
	CollectionType string `json:"CollectionType"`
}

type Item struct {
	ID                   string            `json:"Id"`
	Type                 string            `json:"Type"`
	Name                 string            `json:"Name"`
	RunTimeTicks         int64             `json:"RunTimeTicks"`
	Artists              []string          `json:"Artists"`
	AlbumArtist          string            `json:"AlbumArtist"`
	Album                string            `json:"Album"`
	AlbumID              string            `json:"AlbumId"`
	ImageTags            map[string]string `json:"ImageTags"`
	AlbumPrimaryImageTag string            `json:"AlbumPrimaryImageTag"`
	MediaSources         []MediaSource     `json:"MediaSources"`
}

type MediaSource struct {
	Container    string        `json:"Container"`
	Bitrate      int           `json:"Bitrate"`
	MediaStreams []MediaStream `json:"MediaStreams"`
}

type MediaStream struct {
	Type       string `json:"Type"`
	Codec      string `json:"Codec"`
	BitRate    int    `json:"BitRate"`
	SampleRate int    `json:"SampleRate"`
	BitDepth   int    `json:"BitDepth"`
}

const ticksPerSecond = 10_000_000

func (s MediaSource) AudioStream() (MediaStream, bool) {
	for _, stream := range s.MediaStreams {
		if strings.EqualFold(stream.Type, "Audio") {
			return stream, true
		}
	}
	return MediaStream{}, false
}

func (i Item) convert() media.Track {
	out := media.Track{
		ID:          i.ID,
		Title:       i.Name,
		Artist:      cmp.Or(strings.Join(i.Artists, ", "), i.AlbumArtist),
		AlbumArtist: i.AlbumArtist,
		Album:       i.Album,
		DurationSec: int((i.RunTimeTicks + ticksPerSecond/2) / ticksPerSecond),
		ArtRef:      i.artRef(),
	}
	if len(i.MediaSources) == 0 {
		return out
	}
	source := i.MediaSources[0]
	stream, _ := source.AudioStream()
	container := strings.ToLower(source.Container)
	out.Codec = media.AudioFormat(strings.ToLower(stream.Codec), container)
	out.Container = container
	out.BitrateKbps = (cmp.Or(stream.BitRate, source.Bitrate) + 500) / 1000
	out.SampleRate = stream.SampleRate
	out.BitDepth = stream.BitDepth
	out.FileRef = FilePath(i.ID)
	return out
}

func (i Item) artRef() string {
	if tag := i.ImageTags["Primary"]; tag != "" {
		return ArtPath(i.ID, tag)
	}
	if i.AlbumPrimaryImageTag != "" && i.AlbumID != "" {
		return ArtPath(i.AlbumID, i.AlbumPrimaryImageTag)
	}
	return ""
}

func FilePath(id string) string { return "/Items/" + url.PathEscape(id) + "/File" }

// The tag makes Jellyfin mark the image immutable, so a client that
// revalidates gets a 304 instead of new bytes.
func ArtPath(id, tag string) string {
	return "/Items/" + url.PathEscape(id) + "/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=" + url.QueryEscape(tag)
}
