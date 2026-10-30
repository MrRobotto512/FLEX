package server

import "testing"

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

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
