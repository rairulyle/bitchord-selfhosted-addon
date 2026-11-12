package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"time"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/library"
	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type manifestJSON struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Version      string   `json:"version"`
	Description  string   `json:"description"`
	Resources    []string `json:"resources"`
	Types        []string `json:"types"`
	ContentType  string   `json:"contentType"`
	BitchordAuth bool     `json:"bitchordAuth"`
}

type searchJSON struct {
	Tracks    []trackJSON  `json:"tracks"`
	Albums    []albumJSON  `json:"albums"`
	Artists   []artistJSON `json:"artists"`
	Playlists []any        `json:"playlists"`
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

type albumJSON struct {
	ID         string `json:"id,omitempty"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	ArtworkURL string `json:"artworkURL,omitempty"`
	TrackCount int    `json:"trackCount,omitempty"`
	Year       int    `json:"year,omitempty"`
}

type artistJSON struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name"`
	ArtworkURL string `json:"artworkURL,omitempty"`
	TrackCount int    `json:"trackCount,omitempty"`
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
		Artist:       library.FormatArtists(track.Artist),
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

func signMedia(secret, trackID string, exp int64) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(fmt.Sprintf("%s.%d", trackID, exp)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func verifyMediaSig(secret, trackID, sigStr string, exp int64) bool {
	if exp < time.Now().Unix()-60 {
		return false
	}
	want := signMedia(secret, trackID, exp)
	return subtle.ConstantTimeCompare([]byte(sigStr), []byte(want)) == 1
}

// Bitrate is sent in bits per second: BitChord reads any value above 3000 as
// bits per second, and a hi-res FLAC in kbps would cross that line.
func toStreamJSON(base, secret string, track media.Track) streamJSON {
	fileURL := base + "/file/" + track.ID
	if secret != "" {
		exp := time.Now().Add(30 * time.Minute).Unix()
		sig := signMedia(secret, track.ID, exp)
		fileURL = fmt.Sprintf("%s?exp=%d&sig=%s", fileURL, exp, sig)
	}
	return streamJSON{
		URL:        fileURL,
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
