package server

import (
	"fmt"
	"strings"
)

// mediaSpecs sums up a file for people: "1080p · HEVC 10-bit · 5.1 Dolby
// Digital+ · 2 audio tracks · Subtitles". Lists carry it as "specs" so
// clients can show it without fetching each title's details.
func mediaSpecs(mi *MediaInfo) string {
	if mi == nil {
		return ""
	}
	var parts []string
	v := mi.Video
	if res := resolutionLabel(v.Width, v.Height); res != "" {
		parts = append(parts, res)
	}
	switch v.HDR {
	case "hdr10":
		parts = append(parts, "HDR10")
	case "dolbyvision":
		parts = append(parts, "Dolby Vision")
	case "hlg":
		parts = append(parts, "HLG")
	}
	if c := codecName(videoCodecNames, v.Codec); c != "" {
		if v.BitDepth >= 10 && v.HDR == "" {
			c += " 10-bit" // with HDR it goes without saying
		}
		parts = append(parts, c)
	}
	if a := mainAudio(mi.Audio); a != nil {
		parts = append(parts, strings.TrimSpace(channelsLabel(a.Channels)+" "+codecName(audioCodecNames, a.Codec)))
	}
	if n := len(mi.Audio); n > 1 {
		parts = append(parts, fmt.Sprintf("%d audio tracks", n))
	}
	if len(mi.Subtitles) > 0 {
		parts = append(parts, "Subtitles")
	}
	return strings.Join(parts, " · ")
}

// resolutionLabel names a picture by the usual steps; wide films count by
// width (1920x800 is 1080p).
func resolutionLabel(w, h int) string {
	switch {
	case w <= 0 || h <= 0:
		return ""
	case w >= 3200 || h >= 1800:
		return "4K"
	case w >= 1800 || h >= 1000:
		return "1080p"
	case w >= 1200 || h >= 700:
		return "720p"
	default:
		return "SD"
	}
}

var videoCodecNames = map[string]string{
	"h264": "H.264", "hevc": "HEVC", "av1": "AV1", "vp9": "VP9", "vp8": "VP8", "mpeg4": "MPEG-4",
	"mpeg2video": "MPEG-2", "mpeg1video": "MPEG-1", "vc1": "VC-1", "wmv3": "WMV", "msmpeg4v3": "DivX 3",
}

var audioCodecNames = map[string]string{
	"aac": "AAC", "ac3": "Dolby Digital", "eac3": "Dolby Digital+", "truehd": "Dolby TrueHD", "dts": "DTS",
	"flac": "FLAC", "opus": "Opus", "mp3": "MP3", "mp2": "MP2", "vorbis": "Vorbis", "alac": "ALAC", "wmav2": "WMA",
}

func codecName(names map[string]string, codec string) string {
	if n, ok := names[codec]; ok {
		return n
	}
	if strings.HasPrefix(codec, "pcm_") {
		return "PCM"
	}
	return strings.ToUpper(codec)
}

func channelsLabel(ch int) string {
	switch ch {
	case 0:
		return ""
	case 1:
		return "Mono"
	case 2:
		return "Stereo"
	case 3:
		return "2.1"
	case 6:
		return "5.1"
	case 7:
		return "6.1"
	case 8:
		return "7.1"
	}
	return fmt.Sprintf("%d-channel", ch)
}

// mainAudio is the track players start with: the default one, else the first.
func mainAudio(tracks []AudioInfo) *AudioInfo {
	for i := range tracks {
		if tracks[i].Default {
			return &tracks[i]
		}
	}
	if len(tracks) > 0 {
		return &tracks[0]
	}
	return nil
}
