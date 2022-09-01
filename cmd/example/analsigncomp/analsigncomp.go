package main

import (
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
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
		if info.IsDir() || filepath.Ext(path) != ".hash" {
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
	indir := "/home/gpu/tvideo/fastverifyfaildata/"

	//equal, _ := ffmpeg.CompareSignatureByPath(indir+"1008137096162971958-ewr-trustphase1.hash", indir+"1001979456868321564-ewr-trustphase1.hash")
	//fmt.Println("equal %v\n", equal)

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
	columnheader := []string{"filepath1", "filepath2", "predict_lab", "true_lab"}
	_ = csvrecorder.Write(columnheader)

	okaycount := 0
	failcount := 0

	//for i := 0; i < len(infiles)/2; i++ {
	for i := 0; i < 5000; i++ {
		var linestr []string

		_, filename1 := filepath.Split(infiles[i*2])
		_, filename2 := filepath.Split(infiles[i*2+1])

		linestr = append(linestr, filename1)
		linestr = append(linestr, filename2)
		equal, _ := ffmpeg.CompareSignatureByPath(infiles[i*2], infiles[i*2+1])
		if equal {
			linestr = append(linestr, "1")
			okaycount++
		} else {
			linestr = append(linestr, "0")
			failcount++
		}
		//true label
		linestr = append(linestr, "1")

		csvrecorder.Write(linestr)
	}

	for i := 0; i < 5000; i++ {
		var linestr []string

		_, filename1 := filepath.Split(infiles[i*2])
		randid := rand.Intn(i*2 + 100)
		if randid == i*2 || randid == (i*2+1) || randid >= 5000 {
			i--
			continue
		}

		_, filename2 := filepath.Split(infiles[randid])

		linestr = append(linestr, filename1)
		linestr = append(linestr, filename2)

		fmt.Printf("%v  vs %v \n", filename1, filename2)

		equal, _ := ffmpeg.CompareSignatureByPath(infiles[i*2], infiles[randid])
		if equal {
			linestr = append(linestr, "1")
			failcount++
		} else {
			linestr = append(linestr, "0")
			okaycount++
		}
		//true label
		linestr = append(linestr, "0")

		csvrecorder.Write(linestr)
	}

	fmt.Printf("Test completed! ok:%v  fail:%v", okaycount, failcount)

}
