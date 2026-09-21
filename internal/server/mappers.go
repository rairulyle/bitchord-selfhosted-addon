package server

import (
	"fmt"
	"strconv"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/plex"
)

type manifestJSON struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Resources   []string `json:"resources"`
	Types       []string `json:"types"`
	ContentType string   `json:"contentType"`
}

type searchJSON struct {
	Tracks    []trackJSON `json:"tracks"`
	Albums    []any       `json:"albums"`
	Artists   []any       `json:"artists"`
	Playlists []any       `json:"playlists"`
}

type trackJSON struct {
	ID           string `json:"id"`
	Title        string `json:"title"`
	Artist       string `json:"artist"`
	Album        string `json:"album"`
	Duration     int    `json:"duration"`
	ArtworkURL   string `json:"artworkURL,omitempty"`
	Format       string `json:"format"`
	AudioQuality string `json:"audioQuality"`
}

type streamJSON struct {
	URL        string `json:"url"`
	Format     string `json:"format"`
	Quality    string `json:"quality"`
	Codec      string `json:"codec"`
	Container  string `json:"container"`
	Manifest   string `json:"manifest"`
	SampleRate int    `json:"sampleRate,omitempty"`
	BitDepth   int    `json:"bitDepth,omitempty"`
	Bitrate    int    `json:"bitrate,omitempty"`
}

func toTrackJSON(base string, track library.Track) trackJSON {
	out := trackJSON{
		ID:           track.ID,
		Title:        track.Title,
		Artist:       track.Artist,
		Album:        track.Album,
		Duration:     track.DurationSec,
		Format:       track.Codec,
		AudioQuality: "HIGH",
	}
	if plex.Lossless(track.Codec) {
		out.AudioQuality = "LOSSLESS"
	}
	if track.Thumb != "" {
		out.ArtworkURL = base + "/art/" + track.ID
	}
	return out
}

func toStreamJSON(base string, item plex.Track) (streamJSON, bool) {
	media, part, ok := item.FirstPart()
	if !ok {
		return streamJSON{}, false
	}
	container := media.Container
	if container == "" {
		container = part.Container
	}
	format := plex.AudioFormat(media.AudioCodec, container)
	stream, _ := part.AudioStream()
	kbps := media.Bitrate
	if kbps == 0 {
		kbps = stream.Bitrate
	}
	return streamJSON{
		URL:        base + "/file/" + item.RatingKey,
		Format:     format,
		Quality:    quality(format, kbps, stream),
		Codec:      format,
		Container:  container,
		Manifest:   "none",
		SampleRate: stream.SamplingRate,
		BitDepth:   stream.BitDepth,
		Bitrate:    kbps * 1000,
	}, true
}

func quality(format string, kbps int, stream plex.Stream) string {
	if !plex.Lossless(format) {
		if kbps == 0 {
			return format
		}
		return fmt.Sprintf("%dkbps", kbps)
	}
	out := "lossless"
	if stream.BitDepth > 0 {
		out += fmt.Sprintf(" %d-bit", stream.BitDepth)
	}
	if stream.SamplingRate > 0 {
		out += " " + strconv.FormatFloat(float64(stream.SamplingRate)/1000, 'f', -1, 64) + "kHz"
	}
	return out
}
