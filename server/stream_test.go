package server

import "testing"

func TestPlaysAsIs(t *testing.T) {
	ps5 := profiles["ps5"]
	native := parseCaps("h264,hevc,mpeg4,10bit,2160p,anycontainer,anyaudio")
	uhdHEVC := &MediaInfo{Container: "matroska", BitRate: 4_700_000,
		Video: VideoInfo{Codec: "hevc", Width: 3840, Height: 2160, BitDepth: 10},
		Audio: []AudioInfo{{Index: 1, Codec: "eac3", Channels: 6, Default: true}}}
	hdr := *uhdHEVC
	hdr.Video.HDR = "hdr10"
	fat := *uhdHEVC
	fat.BitRate = 28_000_000
	direct, _ := linkRates(ps5, 12_000_000)

	cases := []struct {
		name    string
		mi      *MediaInfo
		caps    clientCaps
		v, a, c bool
	}{
		{"4K HEVC 10-bit SDR, native player", uhdHEVC, native, true, true, true},
		{"same file, old client", uhdHEVC, clientCaps{}, false, false, false},
		{"HDR needs the server's tone mapping", &hdr, native, false, true, true},
		{"too fat for the link", &fat, native, false, true, true},
		{"ran dry on the client", uhdHEVC, parseCaps("hevc,10bit,2160p,anycontainer,anyaudio,nodirect"), false, true, true},
	}
	for _, c := range cases {
		v, a, ct := playsAsIs(c.mi, -1, ps5, c.caps, direct)
		if v != c.v || a != c.a || ct != c.c {
			t.Errorf("%s: video %v audio %v container %v; want %v %v %v", c.name, v, a, ct, c.v, c.a, c.c)
		}
	}
}

func TestLinkRates(t *testing.T) {
	remote := profiles["remote"]
	if d, tr := linkRates(remote, 0); d != 8_000_000 || tr != 6_000_000 {
		t.Errorf("unmeasured: %d, %d; want the profile's 8M direct, 6M transcode", d, tr)
	}
	if d, tr := linkRates(remote, 12_000_000); d != 7_200_000 || tr != 8_400_000 {
		t.Errorf("12 Mbit/s link: %d, %d", d, tr)
	}
	if _, tr := linkRates(remote, 500_000); tr != minLinkRate {
		t.Errorf("slow link: transcode at %d; want the floor", tr)
	}
	if _, tr := linkRates(remote, 100_000_000); tr != maxLinkRate {
		t.Errorf("fast link: transcode at %d; want the ceiling", tr)
	}
}
