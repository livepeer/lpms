package ffmpeg

import (
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/m3u8"
)

type Format int

const (
	FormatNone Format = iota
	FormatMPEGTS
	FormatMP4
)

type Profile int

const (
	ProfileNone Profile = iota
	ProfileH264Baseline
	ProfileH264Main
	ProfileH264High
	ProfileH264ConstrainedHigh
)

// For additional "special" GOP values
// enumerate backwards from here
const (
	GOPIntraOnly time.Duration = -1

	// Must always be last. Renumber as needed.
	GOPInvalid = -2
)

type VideoCodec int

const (
	H264 VideoCodec = iota
	H265
	VP8
	VP9
)

var VideoCodecName = map[VideoCodec]string{
	H264:   "H.264",
	H265: 	"HEVC",
	VP8:    "VP8",
	VP9:    "VP9",
}

//Standard Profiles:
//1080p60fps: 9000kbps
//1080p30fps: 6000kbps
//720p60fps: 6000kbps
//720p30fps: 4000kbps
//480p30fps: 2000kbps
//360p30fps: 1000kbps
//240p30fps: 700kbps
//144p30fps: 400kbps
type VideoProfile struct {
	Name         string
	Bitrate      string
	Framerate    uint
	FramerateDen uint
	Resolution   string
	AspectRatio  string
	Format       Format
	Profile      Profile
	GOP          time.Duration
	Encoder      VideoCodec
}

//Some sample video profiles
var (
	P720p60fps16x9 = VideoProfile{Name: "P720p60fps16x9", Bitrate: "6000k", Framerate: 60, AspectRatio: "16:9", Resolution: "1280x720"}
	P720p30fps16x9 = VideoProfile{Name: "P720p30fps16x9", Bitrate: "4000k", Framerate: 30, AspectRatio: "16:9", Resolution: "1280x720"}
	P720p25fps16x9 = VideoProfile{Name: "P720p25fps16x9", Bitrate: "3500k", Framerate: 25, AspectRatio: "16:9", Resolution: "1280x720"}
	P720p30fps4x3  = VideoProfile{Name: "P720p30fps4x3", Bitrate: "3500k", Framerate: 30, AspectRatio: "4:3", Resolution: "960x720"}
	P576p30fps16x9 = VideoProfile{Name: "P576p30fps16x9", Bitrate: "1500k", Framerate: 30, AspectRatio: "16:9", Resolution: "1024x576"}
	P576p25fps16x9 = VideoProfile{Name: "P576p25fps16x9", Bitrate: "1500k", Framerate: 25, AspectRatio: "16:9", Resolution: "1024x576"}
	P360p30fps16x9 = VideoProfile{Name: "P360p30fps16x9", Bitrate: "1200k", Framerate: 30, AspectRatio: "16:9", Resolution: "640x360"}
	P360p25fps16x9 = VideoProfile{Name: "P360p25fps16x9", Bitrate: "1000k", Framerate: 25, AspectRatio: "16:9", Resolution: "640x360"}
	P360p30fps4x3  = VideoProfile{Name: "P360p30fps4x3", Bitrate: "1000k", Framerate: 30, AspectRatio: "4:3", Resolution: "480x360"}
	P240p30fps16x9 = VideoProfile{Name: "P240p30fps16x9", Bitrate: "600k", Framerate: 30, AspectRatio: "16:9", Resolution: "426x240"}
	P240p25fps16x9 = VideoProfile{Name: "P240p25fps16x9", Bitrate: "600k", Framerate: 25, AspectRatio: "16:9", Resolution: "426x240"}
	P240p30fps4x3  = VideoProfile{Name: "P240p30fps4x3", Bitrate: "600k", Framerate: 30, AspectRatio: "4:3", Resolution: "320x240"}
	P144p30fps16x9 = VideoProfile{Name: "P144p30fps16x9", Bitrate: "400k", Framerate: 30, AspectRatio: "16:9", Resolution: "256x144"}
	P144p25fps16x9 = VideoProfile{Name: "P144p25fps16x9", Bitrate: "400k", Framerate: 25, AspectRatio: "16:9", Resolution: "256x144"}
)

var VideoProfileLookup = map[string]VideoProfile{
	"P720p60fps16x9": P720p60fps16x9,
	"P720p30fps16x9": P720p30fps16x9,
	"P720p25fps16x9": P720p25fps16x9,
	"P720p30fps4x3":  P720p30fps4x3,
	"P576p30fps16x9": P576p30fps16x9,
	"P576p25fps16x9": P576p25fps16x9,
	"P360p30fps16x9": P360p30fps16x9,
	"P360p25fps16x9": P360p25fps16x9,
	"P360p30fps4x3":  P360p30fps4x3,
	"P240p30fps16x9": P240p30fps16x9,
	"P240p25fps16x9": P240p25fps16x9,
	"P240p30fps4x3":  P240p30fps4x3,
	"P144p30fps16x9": P144p30fps16x9,
}

var FormatExtensions = map[Format]string{
	FormatNone:   ".ts", // default
	FormatMPEGTS: ".ts",
	FormatMP4:    ".mp4",
}
var ExtensionFormats = map[string]Format{
	".ts":  FormatMPEGTS,
	".mp4": FormatMP4,
}

var ProfileParameters = map[Profile]string{
	ProfileNone:                "",
	ProfileH264Baseline:        "baseline",
	ProfileH264Main:            "main",
	ProfileH264High:            "high",
	ProfileH264ConstrainedHigh: "high",
}

func VideoProfileResolution(p VideoProfile) (int, int, error) {
	res := strings.Split(p.Resolution, "x")
	if len(res) < 2 {
		return 0, 0, ErrTranscoderRes
	}
	w, err := strconv.Atoi(res[0])
	if err != nil {
		return 0, 0, err
	}
	h, err := strconv.Atoi(res[1])
	if err != nil {
		return 0, 0, err
	}
	return w, h, nil
}

func VideoProfileToVariantParams(p VideoProfile) m3u8.VariantParams {
	r := p.Resolution
	r = strings.Replace(r, ":", "x", 1)

	bw := p.Bitrate
	bw = strings.Replace(bw, "k", "000", 1)
	b, err := strconv.ParseUint(bw, 10, 32)
	if err != nil {
		glog.Errorf("Error converting %v to variant params: %v", bw, err)
	}
	return m3u8.VariantParams{Bandwidth: uint32(b), Resolution: r}
}

type ByName []VideoProfile

func (a ByName) Len() int      { return len(a) }
func (a ByName) Swap(i, j int) { a[i], a[j] = a[j], a[i] }
func (a ByName) Less(i, j int) bool {
	return a[i].Name > a[j].Name
} //Want to sort in reverse

// func bitrateStrToInt(bitrateStr string) int {
// 	intstr := strings.Replace(bitrateStr, "k", "000", 1)
// 	res, _ := strconv.Atoi(intstr)
// 	return res
// }
