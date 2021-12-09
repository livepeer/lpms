package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

func validRenditions() []string {
	valids := make([]string, len(ffmpeg.VideoProfileLookup))
	for p := range ffmpeg.VideoProfileLookup {
		valids = append(valids, p)
	}
	return valids
}

func main() {
	from := flag.Duration("from", 0, "Skip all frames before that timestamp, from start of the file")
	hevc := flag.Bool("hevc", false, "Use H.265/HEVC for encoding")
	to := flag.Duration("to", 0, "Skip all frames after that timestamp, from start of the file")
	flag.Parse()
	var err error
	args := append([]string{os.Args[0]}, flag.Args()...)
	if len(args) <= 3 {
		panic("Usage: [-hevc] [-from dur] [-to dur] <input file> <output renditions, comma separated> <sw/nv>")
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
			if *hevc {
				p.Encoder = ffmpeg.H265
			}
			profs = append(profs, p)
		}
		return profs
	}
	fname := args[1]
	profiles := str2profs(args[2])
	accel, lbl := str2accel(args[3])

	profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
		opts := []ffmpeg.TranscodeOptions{}
		for i := range profs {
			o := ffmpeg.TranscodeOptions{
				Oname:   fmt.Sprintf("out_%s_%d_out.mp4", lbl, i),
				Profile: profs[i],
				// Uncomment the following to test scene classifier
				// Detector: &ffmpeg.DSceneAdultSoccer,
				Accel: accel,
			}
			o.From = *from
			o.To = *to
			opts = append(opts, o)
		}
		return opts
	}
	options := profs2opts(profiles)

	var dev string
	if accel == ffmpeg.Nvidia {
		if len(args) <= 4 {
			panic("Expected device number")
		}
		dev = args[4]
	}

	ffmpeg.InitFFmpeg()

	t := time.Now()
	fmt.Printf("Setting fname %s encoding %d renditions with %v from %s to %s\n", fname, len(options), lbl, *from, *to)
	res, err := ffmpeg.Transcode3(&ffmpeg.TranscodeOptionsIn{
		Fname:  fname,
		Accel:  accel,
		Device: dev,
	}, options)
	if err != nil {
		panic(err)
	}
	end := time.Now()
	fmt.Printf("profile=input frames=%v pixels=%v\n", res.Decoded.Frames, res.Decoded.Pixels)
	for i, r := range res.Encoded {
		if r.DetectData != nil {
			fmt.Printf("profile=%v frames=%v pixels=%v detectdata=%v\n", profiles[i].Name, r.Frames, r.Pixels, r.DetectData)
		} else {
			fmt.Printf("profile=%v frames=%v pixels=%v\n", profiles[i].Name, r.Frames, r.Pixels)
		}
	}
	fmt.Printf("Transcoding time %0.4v\n", end.Sub(t).Seconds())
}
