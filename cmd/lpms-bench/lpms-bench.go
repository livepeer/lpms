package main

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	//"runtime/pprof"

	"github.com/livepeer/lpms/ffmpeg"
	"github.com/livepeer/m3u8"
)

func validRenditions() []string {
	valids := make([]string, len(ffmpeg.VideoProfileLookup))
	for p := range ffmpeg.VideoProfileLookup {
		valids = append(valids, p)
	}
	return valids
}

func str2profs(inp string) []ffmpeg.VideoProfile {
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

func main() {
	const usage = "Expected: [input file] [output prefix] [# concurrents] [# segments] [profiles] [sw/nv] <nv-device>"
	/*
		cprof, err := os.Create("bench.prof")
		if err != nil {
			panic(err)
		}
		pprof.StartCPUProfile(cprof)
		defer pprof.StopCPUProfile()
	*/
	if len(os.Args) <= 6 {
		panic(usage)
	}
	fname := os.Args[1]
	f, err := os.Open(fname)
	if err != nil {
		panic(err)
	}
	p, _, err := m3u8.DecodeFrom(bufio.NewReader(f), true)
	if err != nil {
		panic(err)
	}
	pl, ok := p.(*m3u8.MediaPlaylist)
	if !ok {
		panic("Expecting media PL")
	}
	//pfx := os.Args[2]
	conc, err := strconv.Atoi(os.Args[3])
	if err != nil {
		panic(err)
	}
	segs, err := strconv.Atoi(os.Args[4])
	if err != nil {
		panic(err)
	}
	profiles := str2profs(os.Args[5])
	accelStr := os.Args[6]
	accel := ffmpeg.Software
	devices := []string{}
	if "nv" == accelStr {
		accel = ffmpeg.Nvidia
		if len(os.Args) <= 7 {
			panic(usage)
		}
		devices = strings.Split(os.Args[7], ",")
	}

	ffmpeg.InitFFmpeg()
	var wg sync.WaitGroup
	dir := path.Dir(fname)
	start := time.Now()
	fmt.Fprintf(os.Stderr, "Program %s Source %s Segments %d Concurrency %d\n", os.Args[0], fname, segs, conc)
	fmt.Println("time,stream,segment,length")
	for i := 0; i < conc; i++ {
		wg.Add(1)
		go func(k int, wg *sync.WaitGroup) {
			tc := ffmpeg.NewTranscoder()
			for j, v := range pl.Segments {
				if j >= segs {
					break
				}
				if v == nil {
					continue
				}
				u := path.Join(dir, v.URI)
				//u := v.URI
				in := &ffmpeg.TranscodeOptionsIn{
					Fname: u,
					Accel: accel,
				}
				if ffmpeg.Software != accel {
					in.Device = devices[k%len(devices)]
				}
				profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
					opts := []ffmpeg.TranscodeOptions{}
					//for n, p := range profs {
					for _, p := range profs {
						o := ffmpeg.TranscodeOptions{
							//Oname: fmt.Sprintf("%s%s_%s_%d_%d_%d.ts", pfx, accelStr, p.Name, n, k, j),
							Oname:        "-",
							Profile:      p,
							Accel:        accel,
							AudioEncoder: ffmpeg.ComponentOptions{Name: "drop"},
							Muxer:        ffmpeg.ComponentOptions{Name: "null"},
						}
						opts = append(opts, o)
					}
					return opts
				}
				out := profs2opts(profiles)
				t := time.Now()
				_, err := tc.Transcode(in, out)
				end := time.Now()
				fmt.Printf("%s,%d,%d,%0.2v\n", end.Format("2006-01-02 15:04:05.999999999"), k, j, end.Sub(t).Seconds())
				if err != nil {
					panic(err)
				}
			}
			tc.StopTranscoder()
			wg.Done()
		}(i, &wg)
		time.Sleep(300 * time.Millisecond)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "Took %v to transcode %v segments\n",
		time.Now().Sub(start).Seconds(), segs)
}