package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// clientCaps is what a client says it decodes itself (?caps=, comma
// separated). The PS5 player decodes on the CPU with FFmpeg: H.264, HEVC and
// MPEG-4 up to 2160p and 10-bit, from any container FFmpeg reads, with any
// audio (it downmixes to stereo), and scales the picture to the screen
// itself. Files within that play as they are when they fit the link. HDR is
// always converted: clients have no tone mapping.
type clientCaps struct {
	HEVC, MPEG4, TenBit, UHD, AnyContainer, AnyAudio bool
	// NoDirect: the client tried the file and its link couldn't keep up
	// (a variable-bitrate peak the average doesn't show): convert it.
	NoDirect bool
}

func parseCaps(s string) clientCaps {
	var c clientCaps
	for _, f := range strings.Split(s, ",") {
		switch strings.TrimSpace(f) {
		case "hevc":
			c.HEVC = true
		case "mpeg4":
			c.MPEG4 = true
		case "10bit":
			c.TenBit = true
		case "2160p":
			c.UHD = true
		case "anycontainer":
			c.AnyContainer = true
		case "anyaudio":
			c.AnyAudio = true
		case "nodirect":
			c.NoDirect = true
		}
	}
	return c
}

const (
	minLinkRate = 1_500_000  // never transcode below this, however slow the link looks
	maxLinkRate = 20_000_000 // 1080p H.264 gains nothing visible above this
)

// linkRates turns the client's measured link speed into the most a file may
// average to play directly (headroom for peaks; the client buffers 30 s)
// and the bitrate to transcode at. Without a measurement the profile's
// fixed numbers apply.
func linkRates(p profile, linkBps int64) (direct, transcode int64) {
	if linkBps <= 0 {
		return p.MaxBitrate + 2_000_000, p.MaxBitrate
	}
	transcode = linkBps * 7 / 10
	transcode = max(minLinkRate, min(transcode, maxLinkRate))
	return linkBps * 6 / 10, transcode
}

// decidePlay picks direct play, remux or transcode for an item (kind is
// "movie" or "episode") and profile.
func (s *Server) decidePlay(ctx context.Context, kind string, id int64, path string, mi *MediaInfo, profileName string, audio, sub int, start float64, upscale bool, caps clientCaps, linkBps int64) (PlayInfo, error) {
	if mi == nil {
		return PlayInfo{}, fmt.Errorf("%s %d has not been probed", kind, id)
	}
	fileURL := fmt.Sprintf("/api/v1/%ss/%d/file", kind, id)
	info := PlayInfo{
		DurationS:  mi.DurationS,
		VideoCodec: mi.Video.Codec,
		AudioCodec: defaultAudio(mi, audio).Codec,
		Width:      mi.Video.Width,
		Height:     mi.Video.Height,
		Container:  mi.Container,
	}

	p, ok := profiles[profileName]
	if !ok || profileName == "direct" {
		info.Method = "direct"
		info.URL = fileURL
		return info, nil
	}

	directRate, transcodeRate := linkRates(p, linkBps)
	if linkBps > 0 { // its own session key: a different bitrate is a different stream
		p.Name = fmt.Sprintf("%s@%dk", p.Name, transcodeRate/1000)
		p.MaxBitrate = transcodeRate
	}
	videoOK, audioOK, containerOK := playsAsIs(mi, audio, p, caps, directRate)

	// Upscaling SD and 720p on the server (Lanczos, then NVENC/x264) looks
	// better than the console stretching the small picture itself; it means
	// a transcode even for files that could play directly.
	upscale = upscale && mi.Video.Height > 0 && mi.Video.Height < p.MaxHeight*9/10
	if upscale {
		videoOK = false
	}

	// Direct play ignores start: the client seeks in the file itself.
	if videoOK && audioOK && containerOK && sub < 0 {
		info.Method = "direct"
		info.URL = fileURL
		return info, nil
	}

	key, err := s.tc.Start(ctx, transcodeRequest{
		Path:    path,
		Media:   mi,
		Profile: p,
		Audio:   audio,
		Sub:     sub,
		Start:   start,
		CopyVid: videoOK && sub < 0,
		Upscale: upscale,
	})
	if err != nil {
		return PlayInfo{}, err
	}
	info.Method = "hls"
	info.URL = "/api/v1/hls/" + key + "/index.m3u8"
	info.Container = "mpegts"
	info.AudioCodec = "aac"
	info.BitRate = transcodeRate
	if !(videoOK && sub < 0) {
		info.VideoCodec = "h264"
		if mi.Video.Height > p.MaxHeight || upscale {
			info.Width = mi.Video.Width * p.MaxHeight / mi.Video.Height / 2 * 2
			info.Height = p.MaxHeight
		}
	}
	info.Upscaled = upscale
	return info, nil
}

// playsAsIs says which parts of a file the client takes as they are: the
// video stream (also what a remux may copy), the audio track and the
// container.
func playsAsIs(mi *MediaInfo, audio int, p profile, caps clientCaps, directRate int64) (video, aud, container bool) {
	codec := mi.Video.Codec
	maxHeight, maxDepth := p.MaxHeight, 8
	if caps.UHD {
		maxHeight = 2160
	}
	if caps.TenBit {
		maxDepth = 10
	}
	video = !caps.NoDirect && (codec == "h264" || (codec == "hevc" && caps.HEVC) ||
		((codec == "mpeg4" || codec == "msmpeg4v3") && caps.MPEG4)) &&
		mi.Video.Height <= maxHeight && mi.Video.BitDepth <= maxDepth && mi.Video.HDR == "" &&
		(mi.BitRate == 0 || mi.BitRate <= directRate)
	a := defaultAudio(mi, audio)
	aud = caps.AnyAudio || (a.Codec == "aac" && a.Channels <= 2)
	container = mi.Container == "mov" || mi.Container == "mp4"
	if caps.AnyContainer {
		container = container || mi.Container == "matroska" || mi.Container == "avi" || mi.Container == "mpegts"
	}
	return video, aud, container
}

// defaultAudio returns the requested track, else the default, else the first.
func defaultAudio(mi *MediaInfo, want int) AudioInfo {
	if len(mi.Audio) == 0 {
		return AudioInfo{}
	}
	for _, a := range mi.Audio {
		if a.Index == want {
			return a
		}
	}
	for _, a := range mi.Audio {
		if a.Default {
			return a
		}
	}
	return mi.Audio[0]
}

// serveFile streams the original file with Range support. http.ServeContent
// does the heavy lifting (Accept-Ranges, 206, If-Range).
func serveFile(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "file missing", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", mimeFor(path))
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}

func mimeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".mp4", ".m4v":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".mov":
		return "video/quicktime"
	case ".ts", ".m2ts":
		return "video/mp2t"
	case ".avi":
		return "video/x-msvideo"
	default:
		return "application/octet-stream"
	}
}
