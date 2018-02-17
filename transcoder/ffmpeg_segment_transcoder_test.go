package transcoder

import (
	"io/ioutil"
	"testing"

	"github.com/livepeer/lpms/ffmpeg"
)

func TestTrans(t *testing.T) {
	testSeg, err := ioutil.ReadFile("./test.ts")
	if err != nil {
		t.Errorf("Error reading test segment: %v", err)
	}

	configs := []ffmpeg.VideoProfile{
		ffmpeg.P144p30fps16x9,
		ffmpeg.P240p30fps16x9,
		ffmpeg.P576p30fps16x9,
	}
	ffmpeg.InitFFmpeg()
	tr := NewFFMpegSegmentTranscoder(configs, "", "./")
	r, err := tr.Transcode(testSeg)
	ffmpeg.DeinitFFmpeg()
	if err != nil {
		t.Errorf("Error transcoding: %v", err)
	}

	if r == nil {
		t.Errorf("Did not get output")
	}

	if len(r) != 3 {
		t.Errorf("Expecting 2 output segments, got %v", len(r))
	}

	if len(r[0]) < 250000 || len(r[0]) > 280000 {
		t.Errorf("Expecting output size to be between 250000 and 280000 , got %v", len(r[0]))
	}

	if len(r[1]) < 280000 || len(r[1]) > 310000 {
		t.Errorf("Expecting output size to be between 280000 and 310000 , got %v", len(r[1]))
	}

	if len(r[2]) < 600000 || len(r[2]) > 700000 {
		t.Errorf("Expecting output size to be between 600000 and 700000, got %v", len(r[2]))
	}
}
