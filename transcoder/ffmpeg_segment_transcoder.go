package transcoder

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/ffmpeg"
)

//SegmentTranscoder transcodes segments individually.  This is a simple wrapper for calling FFMpeg on the command line.
type FFMpegSegmentTranscoder struct {
	tProfiles []ffmpeg.VideoProfile
	workDir   string
}

func NewFFMpegSegmentTranscoder(ps []ffmpeg.VideoProfile, workd string) *FFMpegSegmentTranscoder {
	return &FFMpegSegmentTranscoder{tProfiles: ps, workDir: workd}
}

func (t *FFMpegSegmentTranscoder) Transcode(fname string) ([][]byte, error) {
	prefix := randName()
	//Invoke ffmpeg
	err := ffmpeg.Transcode(fname, t.workDir, t.tProfiles, prefix)
	if err != nil {
		glog.Errorf("Error transcoding: %v", err)
		return nil, err
	}

	dout := make([][]byte, len(t.tProfiles), len(t.tProfiles))
	for i := range t.tProfiles {
		ofile := path.Join(t.workDir, fmt.Sprintf("%sout%v%v", prefix, i, filepath.Base(fname)))
		d, err := ioutil.ReadFile(ofile)
		if err != nil {
			glog.Errorf("Cannot read transcode output: %v", err)
		}
		dout[i] = d
		os.Remove(ofile)
	}

	return dout, nil
}

func randName() string {
	rand.Seed(time.Now().UnixNano())
	x := make([]byte, 10, 10)
	for i := 0; i < len(x); i++ {
		x[i] = byte(rand.Uint32())
	}
	return fmt.Sprintf("%x-", x)
}
