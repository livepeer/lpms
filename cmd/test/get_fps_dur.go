package main

import (
	"os"
	"fmt"
	"text/tabwriter"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

func main() {
	p := "/home/brad/test-videos"
	items, _ := os.ReadDir(p)
	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', tabwriter.Debug)

	fmt.Fprintln(w, "File\tTook (ms)\tFPS\tDur\tACodec")
	for _, item := range items {
		vid_path := p + "/" + item.Name()
		//fmt.Println(item.Name())
		start := time.Now()
		_, mfi, err := ffmpeg.GetCodecInfo(vid_path)
		if err == nil {
			took := time.Since(start).Milliseconds()
			line := fmt.Sprintf("%v\t%v\t%v\t%v\t%v", item.Name(), took, mfi.FPS, mfi.Dur, mfi.Acodec)
			fmt.Fprintln(w, line)
		} else {
			fmt.Printf("error getting codec info: %w\n", err)
		}
	}

	w.Flush()
}
