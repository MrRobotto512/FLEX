package server

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// decidePlay picks direct play, remux or transcode for a movie and profile.
func (s *Server) decidePlay(ctx context.Context, m *Movie, profileName string, audio, sub int, start float64) (PlayInfo, error) {
	if m.Media == nil {
		return PlayInfo{}, fmt.Errorf("movie %d has not been probed", m.ID)
	}
	mi := m.Media
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
		info.URL = fmt.Sprintf("/api/v1/movies/%d/file", m.ID)
		return info, nil
	}

	videoOK := mi.Video.Codec == "h264" && mi.Video.Height <= p.MaxHeight &&
		mi.Video.BitDepth <= 8 && mi.Video.HDR == "" &&
		(mi.BitRate == 0 || mi.BitRate <= p.MaxBitrate+2_000_000)
	audioOK := info.AudioCodec == "aac" && defaultAudio(mi, audio).Channels <= 2
	containerOK := mi.Container == "mov" || mi.Container == "mp4"

	if videoOK && audioOK && containerOK && sub < 0 && start == 0 {
		info.Method = "direct"
		info.URL = fmt.Sprintf("/api/v1/movies/%d/file", m.ID)
		return info, nil
	}

	key, err := s.tc.Start(ctx, transcodeRequest{
		Path:    m.Path,
		Media:   mi,
		Profile: p,
		Audio:   audio,
		Sub:     sub,
		Start:   start,
		CopyVid: videoOK && sub < 0,
	})
	if err != nil {
		return PlayInfo{}, err
	}
	info.Method = "hls"
	info.URL = "/api/v1/hls/" + key + "/index.m3u8"
	info.Container = "mpegts"
	info.AudioCodec = "aac"
	if !(videoOK && sub < 0) {
		info.VideoCodec = "h264"
		if mi.Video.Height > p.MaxHeight {
			info.Width = mi.Video.Width * p.MaxHeight / mi.Video.Height / 2 * 2
			info.Height = p.MaxHeight
		}
	}
	return info, nil
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
