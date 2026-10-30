package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index         int               `json:"index"`
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		Width         int               `json:"width"`
		Height        int               `json:"height"`
		PixFmt        string            `json:"pix_fmt"`
		ColorTransfer string            `json:"color_transfer"`
		RFrameRate    string            `json:"r_frame_rate"`
		AvgFrameRate  string            `json:"avg_frame_rate"`
		Channels      int               `json:"channels"`
		Tags          map[string]string `json:"tags"`
		Disposition   map[string]int    `json:"disposition"`
		SideDataList  []struct {
			Type string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"streams"`
}

// Probe runs ffprobe on one file and distils the parts FLEX cares about.
func Probe(ctx context.Context, ffprobe, path string) (*MediaInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var raw ffprobeOutput
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ffprobe %s: parse: %w", path, err)
	}

	mi := &MediaInfo{
		Container: strings.Split(raw.Format.FormatName, ",")[0],
		Audio:     []AudioInfo{},
		Subtitles: []SubInfo{},
	}
	mi.DurationS, _ = strconv.ParseFloat(raw.Format.Duration, 64)
	mi.Size, _ = strconv.ParseInt(raw.Format.Size, 10, 64)
	mi.BitRate, _ = strconv.ParseInt(raw.Format.BitRate, 10, 64)

	haveVideo := false
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			// skip embedded cover art
			if s.Disposition["attached_pic"] == 1 || haveVideo {
				continue
			}
			haveVideo = true
			mi.Video = VideoInfo{
				Index:    s.Index,
				Codec:    s.CodecName,
				Width:    s.Width,
				Height:   s.Height,
				BitDepth: bitDepth(s.PixFmt),
				HDR:      hdrKind(s.ColorTransfer, s.SideDataList),
				FPS:      parseRate(s.AvgFrameRate, s.RFrameRate),
			}
		case "audio":
			mi.Audio = append(mi.Audio, AudioInfo{
				Index:    s.Index,
				Codec:    s.CodecName,
				Channels: s.Channels,
				Lang:     s.Tags["language"],
				Title:    s.Tags["title"],
				Default:  s.Disposition["default"] == 1,
			})
		case "subtitle":
			mi.Subtitles = append(mi.Subtitles, SubInfo{
				Index:  s.Index,
				Codec:  s.CodecName,
				Lang:   s.Tags["language"],
				Title:  s.Tags["title"],
				Forced: s.Disposition["forced"] == 1,
			})
		}
	}
	if !haveVideo {
		return nil, fmt.Errorf("%s: no video stream", path)
	}
	return mi, nil
}

func bitDepth(pixFmt string) int {
	switch {
	case strings.Contains(pixFmt, "12"):
		return 12
	case strings.Contains(pixFmt, "10"):
		return 10
	default:
		return 8
	}
}

func hdrKind(transfer string, side []struct {
	Type string `json:"side_data_type"`
}) string {
	for _, sd := range side {
		if strings.Contains(sd.Type, "DOVI") || strings.Contains(sd.Type, "Dolby Vision") {
			return "dolbyvision"
		}
	}
	switch transfer {
	case "smpte2084":
		return "hdr10"
	case "arib-std-b67":
		return "hlg"
	}
	return ""
}

// parseRate turns "24000/1001" into 23.976, preferring the average rate.
func parseRate(avg, r string) float64 {
	for _, s := range []string{avg, r} {
		num, den, ok := strings.Cut(s, "/")
		if !ok {
			if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
				return f
			}
			continue
		}
		n, _ := strconv.ParseFloat(num, 64)
		d, _ := strconv.ParseFloat(den, 64)
		if n > 0 && d > 0 {
			return float64(int(n/d*1000+0.5)) / 1000
		}
	}
	return 0
}
