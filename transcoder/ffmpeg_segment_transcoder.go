package transcoder

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

//SegmentTranscoder transcodes segments individually.  This is a simple wrapper for calling FFMpeg on the command line.
type FFMpegSegmentTranscoder struct {
	tProfiles  []ffmpeg.VideoProfile
	ffmpegPath string
	workDir    string
}

func NewFFMpegSegmentTranscoder(ps []ffmpeg.VideoProfile, ffmpegp, workd string) *FFMpegSegmentTranscoder {
	return &FFMpegSegmentTranscoder{tProfiles: ps, ffmpegPath: ffmpegp, workDir: workd}
}

func (t *FFMpegSegmentTranscoder) Transcode(d []byte) ([][]byte, error) {
	//Assume d is in the right format, write it to disk
	inName := randName()
	// outName := fmt.Sprintf("out%v", inName)
	if _, err := os.Stat(t.workDir); os.IsNotExist(err) {
		err := os.Mkdir(t.workDir, 0700)
		if err != nil {
			glog.Errorf("Transcoder cannot create workdir: %v", err)
			return nil, err
		}
	}

	fname := path.Join(t.workDir, inName)
	defer os.Remove(fname)
	if err := ioutil.WriteFile(fname, d, 0644); err != nil {
		glog.Errorf("Transcoder cannot write file: %v", err)
		return nil, err
	}

	//Invoke ffmpeg
	err := ffmpeg.Transcode(fname, t.tProfiles)
	if err != nil {
		glog.Errorf("Error transcoding: %v", err)
		return nil, err
	}

	dout := make([][]byte, len(t.tProfiles), len(t.tProfiles))
	for i, _ := range t.tProfiles {
		d, err := ioutil.ReadFile(path.Join(t.workDir, fmt.Sprintf("out%v%v", i, inName)))
		if err != nil {
			glog.Errorf("Cannot read transcode output: %v", err)
		}
		dout[i] = d
		os.Remove(path.Join(t.workDir, fmt.Sprintf("out%v%v", i, inName)))
	}

	return dout, nil
}

func randName() string {
	rand.Seed(time.Now().UnixNano())
	x := make([]byte, 10, 10)
	for i := 0; i < len(x); i++ {
		x[i] = byte(rand.Uint32())
	}
	return fmt.Sprintf("%x.ts", x)
}
