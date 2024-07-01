package ffmpeg

import (
	"fmt"
	"io"
	"os"
	"testing"
)

func TestTransmuxer_Pipe(t *testing.T) {
	run, dir := setupTest(t)

	run("cp \"$1\"/../data/transmux.ts .")
	ir, iw, err := os.Pipe()
	fname := fmt.Sprintf("%s/transmux.ts", dir)
	_, err = os.Stat(fname)
	if err != nil {
		t.Fatal(err)
		return
	}
	var bytesWritten int64
	go func(iw *os.File) {
		defer iw.Close()
		f, _ := os.Open(fname)
		b, _ := io.Copy(iw, f)
		bytesWritten += b
	}(iw)
	fpipe := fmt.Sprintf("pipe:%d", ir.Fd())
	oname := fmt.Sprintf("%s/test_out.ts", dir)
	in := &TranscodeOptionsIn{
		Fname:       fpipe,
		Transmuxing: true,
	}
	tc := NewTranscoder()
	out := []TranscodeOptions{
		{
			Oname: oname,
			VideoEncoder: ComponentOptions{
				Name: "copy",
			},
			AudioEncoder: ComponentOptions{
				Name: "copy",
			},
			Profile: VideoProfile{Format: FormatNone},
		},
	}
	_, err = tc.Transcode(in, out)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTransmuxer_Join(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
    # run segmenter and sanity check frame counts . Hardcode for now.
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -f hls test.m3u8
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test0.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test1.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test2.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test3.ts | grep nb_read_frames=120
  `
	run(cmd)

	tc := NewTranscoder()

	out := []TranscodeOptions{
		{
			Oname: fmt.Sprintf("%s/out.mp4", dir),
			VideoEncoder: ComponentOptions{
				Name: "copy",
			},
			AudioEncoder: ComponentOptions{
				Name: "copy",
			},
			Profile: VideoProfile{Format: FormatNone},
			Muxer: ComponentOptions{
				Name: "mp4",
				Opts: map[string]string{"movflags": "frag_keyframe+negative_cts_offsets+omit_tfhd_offset+disable_chpl+default_base_moof"},
			},
		},
	}
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:       fmt.Sprintf("%s/test%d.ts", dir, i),
			Transmuxing: true,
		}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Fatal(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Decoded.Frames)
		}
	}
	tc.StopTranscoder()
	cmd = `
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams out.mp4 | grep nb_read_frames=480
  `
	run(cmd)
}

func TestTransmuxer_Discontinuity(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
    # run segmenter and sanity check frame counts . Hardcode for now.
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -f hls test.m3u8
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test0.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test1.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test2.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test3.ts | grep nb_read_frames=120
  `
	run(cmd)

	tc := NewTranscoder()

	out := []TranscodeOptions{
		{
			Oname: fmt.Sprintf("%s/out.mp4", dir),
			VideoEncoder: ComponentOptions{
				Name: "copy",
			},
			AudioEncoder: ComponentOptions{
				Name: "copy",
			},
			Profile: VideoProfile{Format: FormatNone},
			Muxer: ComponentOptions{
				Name: "mp4",
				Opts: map[string]string{"movflags": "frag_keyframe+negative_cts_offsets+omit_tfhd_offset+disable_chpl+default_base_moof"},
			},
		},
	}
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:       fmt.Sprintf("%s/test%d.ts", dir, i),
			Transmuxing: true,
		}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Fatal(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Decoded.Frames)
		}
	}
	tc.Discontinuity()
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:       fmt.Sprintf("%s/test%d.ts", dir, i),
			Transmuxing: true,
		}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Fatal(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Decoded.Frames)
		}
	}

	tc.StopTranscoder()
	cmd = `
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams out.mp4 | grep nb_read_frames=960
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams -show_frames out.mp4 | grep pts=1441410
  `
	run(cmd)
}
