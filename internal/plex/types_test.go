package plex

import (
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func raw(id, title string) Track {
	return Track{
		RatingKey:        id,
		Title:            title,
		GrandparentTitle: "Album Artist",
		ParentTitle:      "Album",
		Duration:         249500,
		ParentThumb:      "/album/thumb",
		Media: []Media{{AudioCodec: "pcm", Container: "wav", Bitrate: 1411,
			Part: []Part{{Key: "/library/parts/" + id + "/file.wav",
				Stream: []Stream{{StreamType: 2, Codec: "pcm", SamplingRate: 44100, BitDepth: 16}}}}}},
	}
}

func TestConvert(t *testing.T) {
	got := raw("7", "Song").convert()
	want := media.Track{ID: "7", Title: "Song", Artist: "Album Artist", AlbumArtist: "Album Artist", Album: "Album",
		DurationSec: 250, Codec: "wav", Container: "wav", BitrateKbps: 1411, SampleRate: 44100, BitDepth: 16,
		FileRef: "/library/parts/7/file.wav", ArtRef: "/album/thumb"}
	if got != want {
		t.Fatalf("convert = %+v, want %+v", got, want)
	}

	withArtist := raw("8", "Song")
	withArtist.OriginalTitle = "Track Artist"
	withArtist.Thumb = "/track/thumb"
	got = withArtist.convert()
	if got.Artist != "Track Artist" || got.AlbumArtist != "Album Artist" || got.ArtRef != "/track/thumb" {
		t.Errorf("track artist or thumb not preferred: %+v", got)
	}

	streamOnly := raw("9", "Song")
	streamOnly.Media[0].Bitrate = 0
	streamOnly.Media[0].Part[0].Stream[0].Bitrate = 320
	if got := streamOnly.convert(); got.BitrateKbps != 320 {
		t.Errorf("bitrate did not fall back to the stream: %+v", got)
	}

	if got := (Track{RatingKey: "9", Title: "No media"}).convert(); got.Playable() {
		t.Error("a track without a part must not be playable")
	}
	if got := raw("", "No key").convert(); got.Playable() {
		t.Error("a track without a ratingKey must not be playable")
	}
}
