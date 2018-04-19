package ffmpeg

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var inputVideo = "../transcoder/test.ts"

func TestFFmpegLength(t *testing.T) {
	InitFFmpeg()
	defer DeinitFFmpeg()
	inp := inputVideo
	// Extract packet count of sample from ffprobe
	// XXX enhance MediaLength to actually return media stats
	cmd := "ffprobe -loglevel quiet -hide_banner "
	cmd += "-select_streams v  -show_streams -count_packets "
	cmd += inp + " | grep -oP 'nb_read_packets=\\K.*$'"
	out, err := exec.Command("bash", "-c", cmd).Output()
	nb_packets, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Error("Could not extract packet count from sample", err)
	}

	// Extract length of test vid (in seconds) from ffprobe
	cmd = "ffprobe -loglevel quiet -hide_banner "
	cmd += "-select_streams v  -show_streams -count_packets "
	cmd += inp + " | grep -oP 'duration=\\K.*$'"
	out, err = exec.Command("bash", "-c", cmd).Output()
	ts_f, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		t.Error("Could not extract timestamp from sample", err)
	}
	ts := int(math.Ceil(ts_f * 1000.0))

	// sanity check baseline numbers
	err = CheckMediaLen(inp, ts, nb_packets)
	if err != nil {
		t.Error("Media sanity check failed")
	}

	err = CheckMediaLen(inp, ts/2, nb_packets)
	if err == nil {
		t.Error("Did not get an error on ts check where one was expected")
	}

	err = CheckMediaLen(inp, ts, nb_packets/2)
	if err == nil {
		t.Error("Did not get an error on nb packets check where one was expected")
	}

}

type FFmpegTest struct {
	Tempdir string
}

func newFFmpegTest(t *testing.T, configs []VideoProfile) (*FFmpegTest, error) {
	d, err := ioutil.TempDir("", "lp-"+t.Name())
	if err != nil {
		return nil, err
	}
	InitFFmpeg()
	return &FFmpegTest{Tempdir: d}, nil
}

func (s *FFmpegTest) Transcode(inp string, ps []VideoProfile) ([]string, error) {
	outputs := make([]string, len(ps))
	for i := range ps {
		outputs[i] = filepath.Join(s.Tempdir, fmt.Sprintf("out%v%v", i, filepath.Base(inp)))
	}
	err := Transcode(inp, s.Tempdir, ps)
	if err != nil {
		return nil, err
	}
	return outputs, nil
}

func (s *FFmpegTest) Close() {
	os.RemoveAll(s.Tempdir)
	DeinitFFmpeg()
}

func TestFFmpegDar(t *testing.T) {
	s, err := newFFmpegTest(t, []VideoProfile{P240p30fps4x3})
	defer s.Close()
	if err != nil {
		t.Error(err)
		return
	}

	getdar := func(inp string) (string, error) {
		cmd := "ffprobe -loglevel quiet -hide_banner "
		cmd += "-show_streams -select_streams v " + inp + " | "
		cmd += "grep -oP 'display_aspect_ratio=\\K.*$'"
		t.Error(cmd)
		out, err := exec.Command("bash", "-c", cmd).Output()
		if err != nil {
			t.Error("Could not extract dar from sample", err)
			return "<nothere>", err
		}
		dar := strings.TrimSpace(string(out))
		return dar, nil
	}
	// truncate input for brevity
	fname := s.Tempdir + "/short.ts"
	cmd := "-i " + inputVideo + " -an -c:v copy -t 1s " + fname
	c := exec.Command("ffmpeg", strings.Split(cmd, " ")...)
	err = c.Run()
	if err != nil {
		t.Error("Unable to truncate input")
	}
	t.Error(cmd)
	// sanity check
	inp_dar, err := getdar(fname)
	if err != nil || inp_dar != "0:1" {
		t.Error("Unexpected DAR when sanity checking: ", inp_dar)
		return
	}
	// set dar
	out, err := s.Transcode(fname, []VideoProfile{P240p30fps4x3})
	if err != nil {
		t.Error("Unable to transcode ", err)
		return
	}
	// check dar
	out_dar, err := getdar(out[0])
	if err != nil || out_dar != P240p30fps4x3.AspectRatio {
		t.Error("Unexpected DAR ", out_dar, err)
		return
	}
	// check nonexistent dar == 0:1
}
