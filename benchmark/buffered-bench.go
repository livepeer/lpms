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

type segData struct {
	tc     *ffmpeg.Transcoder
	seg    *m3u8.MediaSegment
	segNum int
	stream int
}

type segChan chan *segData

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
	fmt.Fprintf(os.Stderr, "Source %s segments %d concurrency %d\n", fname, segs, conc)
	fmt.Println("time,stream,segment,length")

	transcodeLoop := func(gpu string, ch segChan, quit chan struct{}) {
		for {
			select {
			case <-quit:
				return
			case segData := <-ch:

				u := path.Join(dir, segData.seg.URI)
				in := &ffmpeg.TranscodeOptionsIn{
					Fname: u,
					Accel: accel,
				}
				if ffmpeg.Software != accel {
					in.Device = gpu
				}

				profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
					opts := []ffmpeg.TranscodeOptions{}
					//for n, p := range profs {
					for _, p := range profs {
						o := ffmpeg.TranscodeOptions{
							//Oname: fmt.Sprintf("%s%s_%s_%d_%s_%d_%d_%d.ts", pfx, accelStr, p.Name, n, gpu, segData.stream, segData.stream, segData.segNum),
							Oname:        "-",
							Muxer:        ffmpeg.ComponentOptions{Name: "null"},
							Profile:      p,
							Accel:        accel,
							AudioEncoder: ffmpeg.ComponentOptions{Name: "drop"},
						}
						opts = append(opts, o)
					}
					return opts
				}
				out := profs2opts(profiles)
				t := time.Now()
				_, err := segData.tc.Transcode(in, out)
				end := time.Now()

				fmt.Printf("%s,%s,%d,%d,%0.2v\n", end.Format("2006-01-02 15:04:05.999999999"), gpu, segData.stream, segData.segNum, end.Sub(t).Seconds())
				wg.Done()
				if err != nil {
					panic(err)
				}
			}
		}
	}

	ch := []segChan{}
	quit := []chan struct{}{}
	for i, d := range devices {
		ch = append(ch, make(segChan, 1))
		quit = append(quit, make(chan struct{}))
		go transcodeLoop(d, ch[i], quit[i])
	}

	tcoders := []*ffmpeg.Transcoder{}
	for i := 0; i < conc; i++ {
		go func(k int, wg *sync.WaitGroup) {
			device := i % len(devices)
			tc := ffmpeg.NewTranscoder()
			tcoders = append(tcoders, tc)
			for j, v := range pl.Segments {
				if j >= segs {
					break
				}
				if v == nil {
					continue
				}
				wg.Add(1)
				ch[device] <- &segData{seg: v, segNum: j, stream: k, tc: tc}
			}
		}(i, &wg)
		time.Sleep(300 * time.Millisecond)
	}
	wg.Wait()
	fmt.Fprintf(os.Stderr, "Took %v to transcode %v segments\n",
		time.Now().Sub(start).Seconds(), segs)
	for _, v := range quit {
		v <- struct{}{}
	}
	for _, v := range tcoders {
		v.StopTranscoder()
	}
}
