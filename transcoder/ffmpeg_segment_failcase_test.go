// +build nvidia

package transcoder

import (
	"bufio"
	b64 "encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"github.com/livepeer/lpms/ffmpeg"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func selectFile(files *[]string) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Ext(path) != ".txt" {
			return nil
		}
		*files = append(*files, path)
		return nil
	}
}
func getTsFilename(txtpath string) string {
	txtfname := path.Base(txtpath)
	tsfname := ""
	i := strings.Index(txtfname, ".ts-")
	if i > 0 {
		tsfname = txtfname[:i] + ".ts"
	}
	return tsfname
}

func parsingAndStore(t *testing.T, infiles []string, outdir string, inTparam *[][]string) error {

	for i, file := range infiles {
		txtFile, err := os.Open(file)
		if err != nil {
			fmt.Println("Failed in opening .txt file:", i+1, "-", file)
			continue
		}

		reader := bufio.NewReader(txtFile)
		linenum := 0
		var tparam map[string]interface{}
		srcvinfo := ""
		ftsname := getTsFilename(file)
		if len(ftsname) == 0 {
			continue
		}
		ftspath := filepath.Join(outdir, ftsname)
		streamdata := ""
		for {
			linebyte, _, rerr := reader.ReadLine()
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
			}
			linenum++
			if linenum == 1 {
				//fill up target transcoding profile
				json.Unmarshal(linebyte, &tparam)
			} else {
				linestr := string(linebyte)
				if strings.Index(linestr, "{\"duration\"") != 0 {
					streamdata = streamdata + linestr
				} else {
					//fill up source video infomation
					srcvinfo = string(linebyte)
					break
				}
			}
		}
		//write .ts file
		sdec, _ := b64.StdEncoding.DecodeString(string(streamdata))
		err = os.WriteFile(ftspath, sdec, 0644)

		if err == nil {
			profiles := tparam["target_profiles"]
			resjson, _ := json.Marshal(profiles)
			strprofile := string(resjson)
			wrecord := []string{ftspath, strprofile, srcvinfo, file}
			*inTparam = append(*inTparam, wrecord)
			fmt.Println("Succeed in parsing .txt file:", i+1, "-", file)
		} else {
			fmt.Println("Failed in parsing .txt file:", i+1, "-", file)
		}
		txtFile.Close()
	}
	return nil
}

func checkTranscodingFailCase(t *testing.T, inputs [][]string, accel ffmpeg.Acceleration, outdir string, outcsv string) (int, error) {

	failcount := len(inputs)
	fwriter, err := os.Create(outcsv)
	defer fwriter.Close()
	if err != nil {
		return failcount, err
	}
	csvrecorder := csv.NewWriter(fwriter)
	defer csvrecorder.Flush()
	//write header
	columnheader := []string{"video-filepath", "transcoding-pofile", "source-info", "source-path", "error-string"}
	_ = csvrecorder.Write(columnheader)

	ffmpeg.InitFFmpegWithLogLevel(ffmpeg.FFLogWarning)

	for i, indata := range inputs {

		jsonEncodedProfiles := []byte(indata[1])
		profiles, parsingError := ffmpeg.ParseProfiles(jsonEncodedProfiles)
		if parsingError != nil {
			// display the error and continue with other inputs
			fmt.Println("Failed in parsing input:", i, profiles, indata[0])
			indata = append(indata, parsingError.Error())
			csvrecorder.Write(indata)
			continue
		}
		tc := ffmpeg.NewTranscoder()

		profs2opts := func(profs []ffmpeg.VideoProfile) []ffmpeg.TranscodeOptions {
			opts := []ffmpeg.TranscodeOptions{}
			for n, p := range profs {
				oname := fmt.Sprintf("%s/%s_%d_%d.ts", outdir, p.Name, n, i)
				muxer := "mpegts"

				o := ffmpeg.TranscodeOptions{
					Oname:        oname,
					Profile:      p,
					Accel:        accel,
					AudioEncoder: ffmpeg.ComponentOptions{Name: "copy"},
					Muxer:        ffmpeg.ComponentOptions{Name: muxer},
				}
				opts = append(opts, o)
			}
			return opts
		}
		in := &ffmpeg.TranscodeOptionsIn{
			Fname: indata[0],
			Accel: accel,
		}
		out := profs2opts(profiles)
		_, err = tc.Transcode(in, out)

		wrecord := indata
		if err == nil {
			fmt.Println("Succeed in transcoding:", i, profiles, indata[0])
			wrecord = append(wrecord, "success")
			failcount--
		} else {
			fmt.Println("Failed in transcoding:", i, profiles, indata[0])
			wrecord = append(wrecord, err.Error())
		}
		csvrecorder.Write(wrecord)
		tc.StopTranscoder()
	}

	return failcount, nil
}

func TestNvidia_CheckFailCase(t *testing.T) {
	indir := os.Getenv("FAILCASE_PATH")
	if indir == "" {
		t.Skip("Skipping FailCase test; no FAILCASE_PATH set for checking fail case")
	}

	outcsv := "result.csv"
	accel := ffmpeg.Nvidia
	outdir, err := ioutil.TempDir("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outdir)

	var infiles []string
	err = filepath.Walk(indir, selectFile(&infiles))
	if len(infiles) == 0 || err != nil {
		t.Skip("Skipping FailCase test. Can not collect fail case files")
	}
	//srarting parse .txt files and write meta csv file for test
	fmt.Println("Start parsing .txt based files.")
	var inTparams [][]string
	err = parsingAndStore(t, infiles, outdir, &inTparams)

	if err != nil {
		t.Error("Failed in parsing .txt based files.")
	}
	fmt.Println("Start transcoding from failed source files.")

	failcount, lasterr := checkTranscodingFailCase(t, inTparams, accel, outdir, outcsv)

	if lasterr != nil {
		t.Error("Failed in checking fail case files.")
	}
	fmt.Println("Test completed, really failed:", failcount, " / total:", len(inTparams))
}
