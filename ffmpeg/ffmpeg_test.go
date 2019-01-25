package ffmpeg

import (
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestLength(t *testing.T) {
	InitFFmpeg()
	inp := "../transcoder/test.ts"
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

	// check invalid file
	err = CheckMediaLen("nonexistent", ts, nb_packets)
	if err == nil || err.Error() != "No such file or directory" {
		t.Error("Did not get the expected error: ", err)
	}
}

func TestSegmenter_StreamOrdering(t *testing.T) {
	// Ensure segmented output contains [video, audio] streams in that order
	// regardless of stream ordering in the input

	dir, err := ioutil.TempDir("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	InitFFmpeg() // hide some log noise

	// Craft an input that has a subtitle, audio and video stream, in that order
	cmd := `
	    set -eux
	    cd "$0"

		# generate subtitle file
		cat <<- EOF > inp.srt
			1
			00:00:00,000 --> 00:00:01,000
			hi
		EOF

		# borrow the test.ts from the transcoder dir, output with 3 streams
		ffmpeg -loglevel warning -i inp.srt -i "$1/../transcoder/test.ts" -c:a copy -c:v copy -c:s mov_text -t 1 -map 0:s -map 1:a -map 1:v test.mp4

		# some sanity checks. these will exit early on a nonzero code
		# check stream count, then indexes of subtitle, audio and video
		[ $(ffprobe -loglevel warning -i test.mp4 -show_streams | grep index | wc -l) -eq 3 ]
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams s | grep index=0
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams a | grep index=1
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep index=2
	`
	out, err := exec.Command("bash", "-c", cmd, dir, wd).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}

	// actually do the segmentation
	err = RTMPToHLS(dir+"/test.mp4", dir+"/out.m3u8", dir+"/out_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// check stream ordering in output file. Should be video, then audio
	cmd = `
		set -eux
		cd $0
		[ $(ffprobe -loglevel warning -i out_0.ts -show_streams | grep index | wc -l) -eq 2 ]
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams v | grep index=0
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams a | grep index=1
	`
	out, err = exec.Command("bash", "-c", cmd, dir).CombinedOutput()
	if err != nil {
		t.Error(err)
	}
	t.Log(string(out))
}
