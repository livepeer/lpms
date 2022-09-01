package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

func selectFile(files *[]string) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || (filepath.Ext(path) != ".hash" /*&& filepath.Ext(path) != ".hash"*/) {
			return nil
		}
		*files = append(*files, path)
		return nil
	}
}

func main() {
	glog.Info("hi")

	//indir := os.Args[0]
	indir := "/home/gpu/tvideo/fastverifyfaildata/"
	//indir := "/home/gpu/tmp/"
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
		fmt.Printf("%v", vinfo)
		/*if vinfo.Audiosum[0] == 0 && vinfo.Audiosum[1] == 0 && vinfo.Audiosum[2] == 0 && vinfo.Audiosum[3] == 0 {
			missaudio++
		}*/

		sl := strings.Split(infiles[i], "-")

		if len(sl) > 4 {
			newname := sl[0] + "-" + sl[1] + "-" + sl[len(sl)-2] + sl[len(sl)-1]

			e := os.Rename(infiles[i], newname)
			if e != nil {
				fmt.Printf("error %s.", newname)
			}
		}
	}
	fmt.Printf("Missing audio count is %d\n", missaudio)

	fmt.Printf("Task completed!")

}
