package server

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Real ffmpeg 9.0 output from failed encoder tests.
func TestFFmpegError(t *testing.T) {
	cases := []struct{ out, want string }{
		{`[h264_nvenc @ 0000019a4aa52580] Driver does not support the required nvenc API version. Required: 13.1 Found: 13.0
[h264_nvenc @ 0000019a4aa52580] The minimum required Nvidia driver for nvenc is 610.00 or newer
[vost#0:0/h264_nvenc @ 0000019a4aa4fcc0] [enc:h264_nvenc @ 0000019a4a9da280] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.
[vf#0:0 @ 0000019a4aa52b00] Error sending frames to consumers: Function not implemented
[out#0/null @ 0000019a4aa50bc0] Nothing was written into output file, because at least one of its streams received no packets.
`, "Driver does not support the required nvenc API version. Required: 13.1 Found: 13.0 / The minimum required Nvidia driver for nvenc is 610.00 or newer"},
		{`[AMF @ 00000230ef469040] DLL amfrt64.dll failed to open
[h264_amf @ 00000230ed5e2ec0] Failed to create  hardware device context (AMF) : Unknown error occurred
[vost#0:0/h264_amf @ 00000230ed5e0080] [enc:h264_amf @ 00000230ed5a8240] Error while opening encoder - maybe incorrect parameters such as bit_rate, rate, width or height.
`, "DLL amfrt64.dll failed to open / Failed to create  hardware device context (AMF) : Unknown error occurred"},
		{`[out#0/null @ 000001b7077a1b80] Nothing was written into output file, because at least one of its streams received no packets.
`, "Nothing was written into output file, because at least one of its streams received no packets."},
	}
	for _, c := range cases {
		if got := ffmpegError(c.out); got != c.want {
			t.Errorf("ffmpegError:\n got  %q\n want %q", got, c.want)
		}
	}
}

func TestMediaFoundationArgs(t *testing.T) {
	tc := &Transcoder{encoder: "h264_mf"}
	r := transcodeRequest{Path: "x.mkv", Media: &MediaInfo{}, Profile: profiles["ps5"], Audio: -1, Sub: -1}
	args := tc.args(r, t.TempDir())
	has := func(list []string, a, b string) bool {
		for i := 0; i+1 < len(list); i++ {
			if list[i] == a && list[i+1] == b {
				return true
			}
		}
		return false
	}
	if !has(args, "-c:v", "h264_mf") || !has(args, "-hw_encoding", "1") || !has(args, "-profile:v", "100") {
		t.Errorf("h264_mf args missing: %q", args)
	}
	if has(args, "-level", "4.1") || has(args, "-profile:v", "high") {
		t.Errorf("h264_mf can't take a level or a named profile: %q", args)
	}
	var vf string
	for i, a := range args {
		if a == "-vf" {
			vf = args[i+1]
		}
	}
	if want := "format=nv12"; len(vf) < len(want) || vf[len(vf)-len(want):] != want {
		t.Errorf("filters should end in %s: %q", want, vf)
	}

	r.CPUOnly = true
	if args := tc.args(r, t.TempDir()); !has(args, "-c:v", "libx264") {
		t.Errorf("CPUOnly retry should use libx264: %q", args)
	}
}

func TestUpscaleBitrateCap(t *testing.T) {
	tc := &Transcoder{encoder: "libx264"}
	r := transcodeRequest{Path: "x.mkv", Media: &MediaInfo{}, Profile: profiles["ps5"], Audio: -1, Sub: -1}
	if a := tc.videoCodecArgs(r); !contains(a, "12000000") {
		t.Errorf("plain ps5 stream should keep 12 Mbit/s: %q", a)
	}
	r.Upscale = true
	if a := tc.videoCodecArgs(r); !contains(a, "8000000") || contains(a, "12000000") {
		t.Errorf("upscaled stream should be capped at 8 Mbit/s: %q", a)
	}
	r.Profile = profiles["remote"]
	if a := tc.videoCodecArgs(r); !contains(a, "6000000") {
		t.Errorf("remote profile is already below the cap: %q", a)
	}
}

func TestHDRPipelines(t *testing.T) {
	uhd := &MediaInfo{Video: VideoInfo{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10, HDR: "hdr10"}}
	r := transcodeRequest{Path: "tenet.mkv", Media: uhd, Profile: profiles["ps5"], Audio: -1, Sub: -1}
	vf := func(args []string) string {
		for i, a := range args {
			if a == "-vf" {
				return args[i+1]
			}
		}
		return ""
	}
	has := func(s, sub string) bool { return strings.Contains(s, sub) }

	// libplacebo works: one GPU pass, Vulkan set up before the input
	tc := &Transcoder{encoder: "h264_nvenc", hdrGPU: true}
	args := tc.args(r, t.TempDir())
	if !contains(args, "vulkan=vk") || !has(vf(args), "libplacebo=w=1920:h=1080") || !has(vf(args), "tonemapping=bt.2390") ||
		has(vf(args), "zscale") || !contains(args, "h264_nvenc") {
		t.Errorf("HDR on the GPU: %q", args)
	}

	// no libplacebo, NVIDIA: CUDA decodes and shrinks, the CPU tone maps 1080p
	tc = &Transcoder{encoder: "h264_nvenc"}
	args = tc.args(r, t.TempDir())
	if !contains(args, "cuda") || !has(vf(args), "scale_cuda=1920:1080:format=p010le,hwdownload") ||
		!has(vf(args), "tonemap=hable") || has(vf(args), "bwdif") {
		t.Errorf("HDR with CUDA scaling: %q", args)
	}

	// the retry after a GPU failure: everything on the CPU
	r.NoGPU = true
	args = tc.args(r, t.TempDir())
	if contains(args, "vulkan=vk") || has(vf(args), "scale_cuda") || !has(vf(args), "zscale") {
		t.Errorf("HDR CPU retry: %q", args)
	}

	// SDR keeps the all-CUDA path
	r.NoGPU = false
	r.Media = &MediaInfo{Video: VideoInfo{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10}}
	args = (&Transcoder{encoder: "h264_nvenc", hdrGPU: true}).args(r, t.TempDir())
	if has(vf(args), "libplacebo") || !has(vf(args), "bwdif_cuda") {
		t.Errorf("SDR 4K: %q", args)
	}
}

// An encoder chosen in the settings that fails (NVENC on a GTX 10-series
// card with ffmpeg 9) must not be used blindly.
func TestDetectEncoderChosenButBroken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake ffmpeg is a shell script")
	}
	fake := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\ncase \"$*\" in *h264_nvenc*) echo '[h264_nvenc @ 0] The minimum required Nvidia driver for nvenc is 610.00 or newer' >&2; exit 1;; esac\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	enc, tests := DetectEncoder(context.Background(), fake, "nvenc")
	if enc != "h264_qsv" {
		t.Errorf("got %s; want the next encoder that works", enc)
	}
	if !strings.HasPrefix(tests["h264_nvenc"], "chosen in settings, but: The minimum required Nvidia driver") {
		t.Errorf("the reason should be kept: %q", tests["h264_nvenc"])
	}
	if enc, _ := DetectEncoder(context.Background(), fake, "qsv"); enc != "h264_qsv" {
		t.Errorf("a working choice is kept: %s", enc)
	}
}

func TestFitHeight(t *testing.T) {
	cases := [][4]int{{3840, 2160, 1920, 1080}, {3840, 1600, 2592, 1080}, {1920, 800, 1920, 800}, {3836, 1600, 2590, 1080}}
	for _, c := range cases {
		if w, h := fitHeight(c[0], c[1], 1080); w != c[2] || h != c[3] {
			t.Errorf("fitHeight(%d, %d) = %dx%d; want %dx%d", c[0], c[1], w, h, c[2], c[3])
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
