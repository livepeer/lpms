package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

func selectFile(files *[]string) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".ts" {
			return nil
		}
		*files = append(*files, path)
		return nil
	}
}

func main() {
	glog.Info("hi")

	flag.Parse()
	outcsv := "compresult.csv"
	//indir := os.Args[0]
	indir := "/home/gpu/tvideo/analdata/"
	if indir == "" {
		panic("Usage: <input directory>")
	}
	var infiles []string
	err := filepath.Walk(indir, selectFile(&infiles))
	if len(infiles) == 0 || err != nil {
		panic("Can not collect fail case files")
	}

	sort.Strings(infiles)

	fmt.Println("Start comparing.")

	fwriter, err := os.Create(outcsv)
	defer fwriter.Close()
	if err != nil {
		panic("Can not create csv file")
	}
	csvrecorder := csv.NewWriter(fwriter)
	defer csvrecorder.Flush()
	//write header
	columnheader := []string{"filepath1", "filepath2", "width", "height", "bitrate", "packetcount", "timestamp", "audiosum"}
	_ = csvrecorder.Write(columnheader)
	matchingcount := 0

	for i := 0; i < len(infiles)/2; i++ {
		var linestr []string
		var res1, res2 ffmpeg.VideoInfo
		res1, _ = ffmpeg.GetVideoInfoByPath(infiles[i*2])
		res2, _ = ffmpeg.GetVideoInfoByPath(infiles[i*2+1])
		_, filename1 := filepath.Split(infiles[i*2])
		_, filename2 := filepath.Split(infiles[i*2+1])

		if res1.Width >= res2.Width {
			linestr = append(linestr, filename1)
			linestr = append(linestr, filename2)
			ffmpeg.GetDiffInfo(res1, res2, &linestr)
		} else {
			linestr = append(linestr, filename2)
			linestr = append(linestr, filename1)
			ffmpeg.GetDiffInfo(res2, res1, &linestr)
		}
		//bres := ffmpeg.GetCostVideoByPath(infiles[i*2], infiles[i*2+1])
		//sres := fmt.Sprintf("%f", bres)
		//linestr = append(linestr, sres)
		bres, _ := ffmpeg.CompareVideoByPath(infiles[i*2], infiles[i*2+1])
		linestr = append(linestr, strconv.FormatBool(bres))

		csvrecorder.Write(linestr)
	}

	fmt.Printf("Test completed! %d", matchingcount)

}
