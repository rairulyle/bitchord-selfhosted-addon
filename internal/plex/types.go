package plex

import (
	"cmp"

	"github.com/rairulyle/bitchord-selfhosted-addon/internal/media"
)

type Section struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Title string `json:"title"`
}

type Track struct {
	RatingKey        string  `json:"ratingKey"`
	Title            string  `json:"title"`
	OriginalTitle    string  `json:"originalTitle"`
	GrandparentTitle string  `json:"grandparentTitle"`
	ParentTitle      string  `json:"parentTitle"`
	Duration         int     `json:"duration"`
	Thumb            string  `json:"thumb"`
	ParentThumb      string  `json:"parentThumb"`
	Media            []Media `json:"Media"`
}

type Media struct {
	AudioCodec string `json:"audioCodec"`
	Container  string `json:"container"`
	Bitrate    int    `json:"bitrate"`
	Part       []Part `json:"Part"`
}

type Part struct {
	Key       string   `json:"key"`
	Container string   `json:"container"`
	Stream    []Stream `json:"Stream"`
}

type Stream struct {
	StreamType   int    `json:"streamType"`
	Codec        string `json:"codec"`
	SamplingRate int    `json:"samplingRate"`
	BitDepth     int    `json:"bitDepth"`
	Bitrate      int    `json:"bitrate"`
}

const audioStreamType = 2

func (t Track) FirstPart() (Media, Part, bool) {
	for _, media := range t.Media {
		for _, part := range media.Part {
			if part.Key != "" {
				return media, part, true
			}
		}
	}
	return Media{}, Part{}, false
}

func (p Part) AudioStream() (Stream, bool) {
	for _, stream := range p.Stream {
		if stream.StreamType == audioStreamType {
			return stream, true
		}
	}
	return Stream{}, false
}

func (t Track) convert() media.Track {
	out := media.Track{
		ID:          t.RatingKey,
		Title:       t.Title,
		Artist:      cmp.Or(t.OriginalTitle, t.GrandparentTitle),
		AlbumArtist: t.GrandparentTitle,
		Album:       t.ParentTitle,
		DurationSec: (t.Duration + 500) / 1000,
		ArtRef:      cmp.Or(t.Thumb, t.ParentThumb),
	}
	m, part, ok := t.FirstPart()
	if !ok {
		return out
	}
	container := cmp.Or(m.Container, part.Container)
	stream, _ := part.AudioStream()
	out.Codec = media.AudioFormat(m.AudioCodec, container)
	out.Container = container
	out.BitrateKbps = cmp.Or(m.Bitrate, stream.Bitrate)
	out.SampleRate = stream.SamplingRate
	out.BitDepth = stream.BitDepth
	out.FileRef = part.Key
	return out
}
