package server

import "testing"

func TestMediaSpecs(t *testing.T) {
	cases := []struct {
		mi   *MediaInfo
		want string
	}{
		{nil, ""},
		{&MediaInfo{Video: VideoInfo{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10, HDR: "hdr10"},
			Audio:     []AudioInfo{{Codec: "aac", Channels: 2}, {Codec: "eac3", Channels: 6, Default: true}},
			Subtitles: []SubInfo{{Codec: "subrip"}}},
			"4K · HDR10 · HEVC · 5.1 Dolby Digital+ · 2 audio tracks · Subtitles"},
		{&MediaInfo{Video: VideoInfo{Codec: "hevc", Width: 1920, Height: 1040, BitDepth: 10},
			Audio: []AudioInfo{{Codec: "aac", Channels: 6}}},
			"1080p · HEVC 10-bit · 5.1 AAC"},
		{&MediaInfo{Video: VideoInfo{Codec: "h264", Width: 1920, Height: 800, BitDepth: 8},
			Audio: []AudioInfo{{Codec: "ac3", Channels: 2}}},
			"1080p · H.264 · Stereo Dolby Digital"},
		{&MediaInfo{Video: VideoInfo{Codec: "mpeg4", Width: 720, Height: 304}, Audio: []AudioInfo{{Codec: "mp3", Channels: 1}}},
			"SD · MPEG-4 · Mono MP3"},
		{&MediaInfo{Video: VideoInfo{Codec: "h264", Width: 1280, Height: 536}, Audio: []AudioInfo{{Codec: "pcm_s16le", Channels: 8}}},
			"720p · H.264 · 7.1 PCM"},
		{&MediaInfo{Video: VideoInfo{Codec: "prores", Width: 1920, Height: 1080, HDR: "dolbyvision"}},
			"1080p · Dolby Vision · PRORES"},
	}
	for _, c := range cases {
		if got := mediaSpecs(c.mi); got != c.want {
			t.Errorf("mediaSpecs = %q; want %q", got, c.want)
		}
	}
}
