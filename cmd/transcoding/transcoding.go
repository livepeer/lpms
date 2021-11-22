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
	var err error
	if len(os.Args) <= 3 {
		panic("Usage: <input file> <output renditions, comma separated> <sw/nv> <from> <to>")
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
	fname := os.Args[1]
	profiles := str2profs(os.Args[2])
	accel, lbl := str2accel(os.Args[3])
	var from, to time.Duration
	if len(os.Args) > 4 {
		from, err = time.ParseDuration(os.Args[4])
		if err != nil {
			panic(err)
		}
	}
	if len(os.Args) > 5 {
		to, err = time.ParseDuration(os.Args[5])
		if err != nil {
			panic(err)
		}
	}

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
			o.From = from
			o.To = to
			opts = append(opts, o)
		}
		return opts
	}
	options := profs2opts(profiles)

	var dev string
	if accel == ffmpeg.Nvidia {
		if len(os.Args) <= 4 {
			panic("Expected device number")
		}
		dev = os.Args[4]
	}

	ffmpeg.InitFFmpeg()

	t := time.Now()
	fmt.Printf("Setting fname %s encoding %d renditions with %v\n", fname, len(options), lbl)
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
