package ffmpeg

import (
	"io/ioutil"
	"os"
	"os/exec"
	"testing"
)

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
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}
}

func TestTranscoder_UnevenRes(t *testing.T) {
	// Ensure transcoding still works on input with uneven resolutions
	// and that aspect ratio is maintained

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

	// Craft an input with an uneven res
	cmd := `
	    set -eux
	    cd "$0"

		# borrow the test.ts from the transcoder dir, output with 123x456 res
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:a copy -c:v mpeg4 -s 123x456 test.mp4

		# sanity check resulting resolutions
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep width=123
		ffprobe -loglevel warning -i test.mp4 -show_streams -select_streams v | grep height=456

		# and generate another sample with an odd value in the larger dimension
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:a copy -c:v mpeg4 -s 123x457 test_larger.mp4
		ffprobe -loglevel warning -i test_larger.mp4 -show_streams -select_streams v | grep width=123
		ffprobe -loglevel warning -i test_larger.mp4 -show_streams -select_streams v | grep height=457

	`
	out, err := exec.Command("bash", "-c", cmd, dir, wd).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}

	err = Transcode(dir+"/test.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	err = Transcode(dir+"/test_larger.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions
	cmd = `
		set -eux
		cd "$0"
		ffprobe -loglevel warning -show_streams -select_streams v out0test.mp4 | grep width=64
		ffprobe -loglevel warning -show_streams -select_streams v out0test.mp4 | grep height=240
		ffprobe -loglevel warning -show_streams -select_streams v out0test_larger.mp4 | grep width=64
		ffprobe -loglevel warning -show_streams -select_streams v out0test_larger.mp4 | grep height=240
	`
	out, err = exec.Command("bash", "-c", cmd, dir).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}

	// Transpose input and do the same checks as above.
	cmd = `
		set -eux
		cd "$0"
		ffmpeg -loglevel warning -i test.mp4 -c:a copy -c:v mpeg4 -vf transpose transposed.mp4

		# sanity check resolutions
		ffprobe -loglevel warning -show_streams -select_streams v transposed.mp4 | grep width=456
		ffprobe -loglevel warning -show_streams -select_streams v transposed.mp4 | grep height=123
	`
	out, err = exec.Command("bash", "-c", cmd, dir, wd).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}

	err = Transcode(dir+"/transposed.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions for transposed input
	cmd = `
		set -eux
		cd "$0"
		ffprobe -loglevel warning -show_streams -select_streams v out0transposed.mp4 | grep width=426
		ffprobe -loglevel warning -show_streams -select_streams v out0transposed.mp4 | grep height=114
	`
	run(cmd)

	// check special case of square resolutions
	cmd = `
		set -eux
		cd "$0"
		ffmpeg -loglevel warning -i test.mp4 -c:a copy -c:v mpeg4 -s 123x123 square.mp4

		# sanity check resolutions
		ffprobe -loglevel warning -show_streams -select_streams v square.mp4 | grep width=123
		ffprobe -loglevel warning -show_streams -select_streams v square.mp4 | grep height=123
	`
	run(cmd)

	err = Transcode(dir+"/square.mp4", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Check output resolutions are still square
	cmd = `
		set -eux
		cd "$0"
		ls
		ffprobe -loglevel warning -i out0square.mp4 -show_streams -select_streams v | grep width=426
		ffprobe -loglevel warning -i out0square.mp4 -show_streams -select_streams v | grep height=426
	`
	out, err = exec.Command("bash", "-c", cmd, dir).CombinedOutput()
	t.Log(string(out))
	if err != nil {
		t.Error(err)
	}

	// TODO set / check sar/dar values?
}
