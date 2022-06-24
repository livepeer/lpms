package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

func selectFile(files *[]string) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (filepath.Ext(path) != ".ts" /*&& filepath.Ext(path) != ".hash"*/) {
			return nil
		}
		*files = append(*files, path)
		return nil
	}
}

func main() {
	glog.Info("hi")

	flag.Parse()

	//indir := os.Args[0]
	//indir := "/home/gpu/tvideo/fastverifyfaildata1/"
	indir := "/home/gpu/tmp/"
	if indir == "" {
		panic("Usage: <input directory>")
	}
	var infiles []string
	err := filepath.Walk(indir, selectFile(&infiles))
	if len(infiles) == 0 || err != nil {
		panic("Can not collect fail case files")
	}

	sort.Strings(infiles)

	fmt.Println("Task starting.")
	missaudio := 0

	for i := 0; i < len(infiles); i++ {

		var vinfo ffmpeg.VideoInfo

		vinfo, _ = ffmpeg.GetVideoInfoByPath(infiles[i])
		if vinfo.Audiosum[0] == 0 && vinfo.Audiosum[1] == 0 && vinfo.Audiosum[2] == 0 && vinfo.Audiosum[3] == 0 {
			missaudio++
		}
		/*
			sl := strings.Split(infiles[i], "-")
			newname := sl[0] + "-" + sl[len(sl)-2] + sl[len(sl)-1]

			e := os.Rename(infiles[i], newname)
			if e != nil {
				log.Fatal(e)
			}
		*/
	}
	fmt.Printf("Missing audio count is %d", missaudio)

	fmt.Printf("Task completed!")

}
