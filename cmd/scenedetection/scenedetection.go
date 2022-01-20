package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

func validRenditions() []string {
	valids := make([]string, len(ffmpeg.VideoProfileLookup))
	for p, _ := range ffmpeg.VideoProfileLookup {
		valids = append(valids, p)
	}
	return valids
}

func main() {
	if len(os.Args) <= 4 {
		//0,1 input.mp4 P720p25fps16x9,P720p30fps4x3 nv 0
		panic("Usage:<dnn init deviceid> <input file> <output renditions, comma separated> <sw/nv>")
	}
	str2accel := func(inp string) (ffmpeg.Acceleration, string) {
		if inp == "nv" {
			return ffmpeg.Nvidia, "nv"
		}
		return ffmpeg.Software, "sw"
	}
	str2profs := func(inp string) []ffmpeg.VideoProfile {
		profs := []ffmpeg.VideoProfile{}
		strs := strings.Split(inp, ",")
		for _, k := range strs {
			p, ok := ffmpeg.VideoProfileLookup[k]
			if !ok {
				panic(fmt.Sprintf("Invalid rendition %s. Valid renditions are:\n%s", k, validRenditions()))
			}
			profs = append(profs, p)
		}
		return profs
	}
	deviceid := os.Args[1]
	fname := os.Args[2]
	profiles := str2profs(os.Args[3])
	accel, lbl := str2accel(os.Args[4])

	var dev string
	if accel == ffmpeg.Nvidia {
		if len(os.Args) <= 5 {
			panic("Expected device number")
		}
		dev = os.Args[5]
	}
	ffmpeg.InitFFmpeg()

	t := time.Now()
	tc, err := ffmpeg.NewTranscoderWithDetector(&ffmpeg.DSceneAdultSoccer, deviceid)
	defer tc.StopTranscoder()
	end := time.Now()

	if err != nil {
		panic(err)
	}
	fmt.Printf("InitFFmpegWithDetectorProfile time %0.4v\n", end.Sub(t).Seconds())

	profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
		opts := []ffmpeg.TranscodeOptions{}
		for i := range profs {
			o := ffmpeg.TranscodeOptions{
				Oname:   fmt.Sprintf("out_%s_%d_out.mkv", lbl, i),
				Profile: profs[i],
				Accel:   accel,
			}
			opts = append(opts, o)
		}
		//add detection profile
		detectorProfile := ffmpeg.DSceneAdultSoccer
		detectorProfile.SampleRate = 100
		o := ffmpeg.TranscodeOptions{
			Oname:    fmt.Sprintf("out_dnn.mkv"),
			Profile:  ffmpeg.P144p30fps16x9,
			Detector: &detectorProfile,
			Accel:    accel,
		}
		opts = append(opts, o)
		return opts
	}
	options := profs2opts(profiles)

	t = time.Now()
	fmt.Printf("Setting fname %s encoding %d renditions with %v\n", fname, len(options), lbl)
	res, err := tc.Transcode(&ffmpeg.TranscodeOptionsIn{
		Fname:  fname,
		Accel:  accel,
		Device: dev,
	}, options)
	if err != nil {
		panic(err)
	}
	end = time.Now()
	fmt.Printf("profile=input frames=%v pixels=%v\n", res.Decoded.Frames, res.Decoded.Pixels)
	for i, r := range res.Encoded {
		if r.DetectData != nil {
			fmt.Printf("profile=%v frames=%v pixels=%v detectdata= %v\n", options[i].Profile, r.Frames, r.Pixels, r.DetectData)
		} else {
			fmt.Printf("profile=%v frames=%v pixels=%v\n", options[i].Profile, r.Frames, r.Pixels)
		}
	}
	fmt.Printf("Transcoding time %0.4v\n", end.Sub(t).Seconds())
}
