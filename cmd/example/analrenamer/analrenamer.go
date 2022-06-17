package main

import (
	"flag"
	"fmt"
	"log"
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

	for i := 0; i < len(infiles); i++ {

		var vinfo ffmpeg.VideoInfo

		vinfo, _ = ffmpeg.GetVideoInfoByPath(infiles[i])
		fmt.Printf("%v - info %v", i, vinfo)
		sl := strings.Split(infiles[i], "-")
		newname := sl[0] + "-" + sl[len(sl)-2] + sl[len(sl)-1]

		e := os.Rename(infiles[i], newname)
		if e != nil {
			log.Fatal(e)
		}
	}

	fmt.Printf("Task completed!")

}
