package server

import (
	"fmt"
	"strconv"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
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

func toTrackJSON(base string, track media.Track) trackJSON {
	out := trackJSON{
		ID:           track.ID,
		Title:        track.Title,
		Artist:       track.Artist,
		Album:        track.Album,
		Duration:     track.DurationSec,
		Format:       track.Codec,
		AudioQuality: "HIGH",
	}
	if media.Lossless(track.Codec) {
		out.AudioQuality = "LOSSLESS"
	}
	if track.ArtRef != "" {
		out.ArtworkURL = base + "/art/" + track.ID
	}
	return out
}

// Bitrate is sent in bits per second: BitChord reads any value above 3000 as
// bits per second, and a hi-res FLAC in kbps would cross that line.
func toStreamJSON(base string, track media.Track) streamJSON {
	return streamJSON{
		URL:        base + "/file/" + track.ID,
		Format:     track.Codec,
		Quality:    quality(track),
		Codec:      track.Codec,
		Container:  track.Container,
		Manifest:   "none",
		SampleRate: track.SampleRate,
		BitDepth:   track.BitDepth,
		Bitrate:    track.BitrateKbps * 1000,
	}
}

func quality(track media.Track) string {
	if !media.Lossless(track.Codec) {
		if track.BitrateKbps == 0 {
			return track.Codec
		}
		return fmt.Sprintf("%dkbps", track.BitrateKbps)
	}
	out := "lossless"
	if track.BitDepth > 0 {
		out += fmt.Sprintf(" %d-bit", track.BitDepth)
	}
	if track.SampleRate > 0 {
		out += " " + strconv.FormatFloat(float64(track.SampleRate)/1000, 'f', -1, 64) + "kHz"
	}
	return out
}
