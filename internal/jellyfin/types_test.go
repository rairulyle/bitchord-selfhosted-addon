package jellyfin

import (
	"testing"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

func raw(id, title string) Item {
	return Item{
		ID:           id,
		Name:         title,
		RunTimeTicks: 2495000000,
		Artists:      []string{"Track Artist", "Guest"},
		AlbumArtist:  "Album Artist",
		Album:        "Album",
		AlbumID:      "album1",
		ImageTags:    map[string]string{"Primary": "tag1"},
		MediaSources: []MediaSource{{Container: "FLAC", Bitrate: 900000,
			MediaStreams: []MediaStream{{Type: "Video", Codec: "mjpeg"}, {Type: "Audio", Codec: "FLAC", BitRate: 1875400, SampleRate: 48000, BitDepth: 24}}}},
	}
}

func TestConvert(t *testing.T) {
	got := raw("7", "Song").convert()
	want := media.Track{ID: "7", Title: "Song", Artist: "Track Artist, Guest", AlbumArtist: "Album Artist", Album: "Album",
		DurationSec: 250, Codec: "flac", Container: "flac", BitrateKbps: 1875, SampleRate: 48000, BitDepth: 24,
		FileRef: "/Items/7/File", ArtRef: "/Items/7/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=tag1"}
	if got != want {
		t.Fatalf("convert = %+v, want %+v", got, want)
	}

	albumArt := raw("8", "Song")
	albumArt.Artists = nil
	albumArt.ImageTags = nil
	albumArt.AlbumPrimaryImageTag = "atag"
	got = albumArt.convert()
	if got.Artist != "Album Artist" || got.ArtRef != "/Items/album1/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=atag" {
		t.Errorf("artist or album art fallback: %+v", got)
	}

	noArt := raw("9", "Song")
	noArt.ImageTags = nil
	if got := noArt.convert(); got.ArtRef != "" {
		t.Errorf("no art must give an empty ArtRef: %+v", got)
	}

	wav := raw("10", "Song")
	wav.MediaSources[0].Container = "wav"
	wav.MediaSources[0].MediaStreams[1] = MediaStream{Type: "Audio", Codec: "pcm", SampleRate: 44100, BitDepth: 16}
	if got := wav.convert(); got.Codec != "wav" || got.BitrateKbps != 900 {
		t.Errorf("pcm in wav or source bitrate fallback: %+v", got)
	}

	noStream := raw("12", "Song")
	noStream.MediaSources[0].MediaStreams = nil
	if got := noStream.convert(); !got.Playable() || got.Codec != "flac" || got.BitrateKbps != 900 || got.SampleRate != 0 {
		t.Errorf("a source without an audio stream must still play from its container: %+v", got)
	}

	if got := (Item{ID: "11", Name: "No media"}).convert(); got.Playable() || got.DurationSec != 0 {
		t.Errorf("a track without a media source must not be playable: %+v", got)
	}
}

func TestPathsEscapeIds(t *testing.T) {
	if got := FilePath("a/b"); got != "/Items/a%2Fb/File" {
		t.Errorf("FilePath = %s", got)
	}
	if got := ArtPath("a b", "t&g"); got != "/Items/a%20b/Images/Primary?fillWidth=600&fillHeight=600&quality=90&tag=t%26g" {
		t.Errorf("ArtPath = %s", got)
	}
}
