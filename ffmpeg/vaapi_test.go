// +build vaapi

package ffmpeg

import (
	"fmt"
	"os"
	"testing"
)

func TestVaapi_Transcoding(t *testing.T) {
	// XXX what is missing is a way to verify these are *actually* running on GPU!

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # set up initial input; truncate test.ts file
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test.ts
  `
	run(cmd)

	var err error
	fname := dir + "/test.ts"
	oname := dir + "/out.ts"
	prof := P240p30fps16x9

	// hw enc + dec
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: VAAPI,
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   oname,
			Profile: prof,
			Accel:   VAAPI,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// software transcode for image quality check
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Software,
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   dir + "/sw.ts",
			Profile: prof,
			Accel:   Software,
		},
	})
	if err != nil {
		t.Error(err)
	}

	cmd = `
    # compare using ssim and generate stats file
    ffmpeg -loglevel warning -i out.ts -i sw.ts -lavfi '[0:v][1:v]ssim=stats.log' -f null -
    # check image quality; ensure that no more than 5 frames have ssim < 0.95
    grep -Po 'All:\K\d+.\d+' stats.log | awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'
  `
	run(cmd)

}

func TestVaapi_Transcoding_Multiple(t *testing.T) {

	// Tests multiple encoding profiles.
	// May be skipped in 'short' mode.

	if testing.Short() {
		t.Skip("Skipping encoding multiple profiles")
	}

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # set up initial input; truncate test.ts file
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test.ts
    # sanity check input dimensions for resolution pass through test
    ffprobe -loglevel warning -show_streams -select_streams v test.ts | grep width=1280
    ffprobe -loglevel warning -show_streams -select_streams v test.ts | grep height=720
  `
	run(cmd)

	fname := dir + "/test.ts"
	prof := P240p30fps16x9
	orig := P720p30fps16x9

	mkoname := func(i int) string { return fmt.Sprintf("%s/%d.ts", dir, i) }
	out := []TranscodeOptions{
		TranscodeOptions{
			Oname: mkoname(0),
			// pass through resolution has different behavior;
			// basically bypasses the scale filter
			Profile: orig,
			Accel:   VAAPI,
		},
		TranscodeOptions{
			Oname:   mkoname(1),
			Profile: prof,
			Accel:   VAAPI,
		},
		// another gpu rendition for good measure?
		TranscodeOptions{
			Oname:   mkoname(2),
			Profile: prof,
			Accel:   VAAPI,
		},
	}

	// generate the above outputs with the given decoder
	test := func(decoder Acceleration) {
		err := Transcode2(&TranscodeOptionsIn{
			Fname: fname,
			Accel: decoder,
		}, out)
		if err != nil {
			t.Error(err)
		}
		// XXX should compare ssim image quality of results
	}
	test(VAAPI)
}

func TestVaapi_Devices(t *testing.T) {

	// XXX need to verify these are running on the correct GPU
	//     not just that the code runs

	device := os.Getenv("GPU_DEVICE")
	if device == "" {
		t.Skip("Skipping device specific tests; no GPU_DEVICE set")
	}

	_, dir := setupTest(t)
	defer os.RemoveAll(dir)

	var err error
	fname := "../transcoder/test.ts"
	oname := dir + "/out.ts"
	prof := P240p30fps16x9

	// hw enc + dec
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  VAAPI,
		Device: device,
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   oname,
			Profile: prof,
			Accel:   VAAPI,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// TODO: Test on machine having separate GPU
	//// hw enc + hw dec, separate devices
	//err = Transcode2(&TranscodeOptionsIn{
	//	Fname:  fname,
	//	Accel:  VAAPI,
	//	Device: "/dev/dri/renderD128",
	//}, []TranscodeOptions{
	//	TranscodeOptions{
	//		Oname:   oname,
	//		Profile: prof,
	//		Accel:   VAAPI,
	//		Device:  "/dev/dri/renderD129",
	//	},
	//})
	//if err != ErrTranscoderInp {
	//	t.Error(err)
	//}

	// invalid device for decoding
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  VAAPI,
		Device: "9999",
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   oname,
			Profile: prof,
			Accel:   VAAPI,
		},
	})
	if err == nil || err.Error() != "Invalid argument" {
		t.Error(fmt.Errorf(fmt.Sprintf("\nError being: '%v'\n", err)))
	}

	// invalid device for encoding
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: VAAPI,
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   oname,
			Profile: prof,
			Accel:   VAAPI,
			Device:  "9999",
		},
	})
	if err == nil || err.Error() != "TranscoderInvalidInput" {
		t.Error(fmt.Errorf(fmt.Sprintf("\nError being: '%v'\n", err)))
	}
}

func TestVaapi_DrainFilters(t *testing.T) {
	// Going from low fps to high fps has the potential to retain lots of
	// GPU surfaces. Ensure this is not a problem anymore.
	// May be skipped in 'short' mode.

	if testing.Short() {
		t.Skip("Skipping encoding multiple profiles")
	}

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # set up initial input; truncate test.ts file
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test.ts
  `
	run(cmd)

	prof := P240p30fps16x9
	prof.Framerate = 100

	fname := dir + "/test.ts"
	oname := dir + "/out.ts"

	// hw enc + dec
	err := Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: VAAPI,
	}, []TranscodeOptions{
		TranscodeOptions{
			Oname:   oname,
			Profile: prof,
			Accel:   VAAPI,
		},
	})
	if err != nil {
		t.Error(err)
	}

	cmd = `
    # sanity check with ffmpeg itself
    ffmpeg -loglevel warning -i test.ts -c:a copy -c:v libx264 -vf fps=100 -vsync 0 ffmpeg-out.ts
    ffprobe -loglevel warning -show_streams -select_streams v -count_frames ffmpeg-out.ts > ffmpeg.out
    grep nb_read_frames ffmpeg.out > ffmpeg-read-frames.out
    grep duration= ffmpeg.out > ffmpeg-duration.out
    # ensure output has correct fps and duration
    ffprobe -loglevel warning -show_streams -select_streams v -count_frames out.ts > probe.out
    grep nb_read_frames probe.out > read-frames.out
    diff -u ffmpeg-read-frames.out read-frames.out
    grep duration= probe.out > duration.out
    diff -u ffmpeg-duration.out duration.out
    # actual values - these are not *that* important as long as they're
    # reasonable and match ffmpeg's
    grep nb_read_frames=102 probe.out
    grep duration=1.0200 probe.out
  `
	run(cmd)

}

func TestVaapi_CountFrames(t *testing.T) {
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

	// Test decoding
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:  fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel:  VAAPI,
			Device: "/dev/dri/renderD128",
		}
		res, err := tc.Transcode(in, nil)
		if err != nil {
			t.Error(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Decoded.Frames)
		}
	}
	tc.StopTranscoder()
}

func TestVaapi_CountEncodedFrames(t *testing.T) {
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

	// Test decoding
	for i := 0; i < 1; i++ {
		in := &TranscodeOptionsIn{
			Fname:  fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel:  VAAPI,
			Device: "/dev/dri/renderD128",
		}
		p60fps := P144p30fps16x9
		p60fps.Framerate = 60
		p120fps := P144p30fps16x9
		p120fps.Framerate = 120
		out := []TranscodeOptions{TranscodeOptions{
			Oname:   fmt.Sprintf("%s/out_30fps_%d.ts", dir, i),
			Profile: P144p30fps16x9,
			Accel:   VAAPI,
			Device:  "/dev/dri/renderD128",
		}, TranscodeOptions{
			Oname:   fmt.Sprintf("%s/out_60fps_%d.ts", dir, i),
			Profile: p60fps,
			Accel:   VAAPI,
			Device:  "/dev/dri/renderD128",
		}, TranscodeOptions{
			Oname:   fmt.Sprintf("%s/out_120fps_%d.ts", dir, i),
			Profile: p120fps,
			Accel:   VAAPI,
			Device:  "/dev/dri/renderD128",
		}}

		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res.Encoded[0].Frames != 60 {
			t.Error(in.Fname, " Mismatched frame count: expected 60 got ", res.Encoded[0].Frames)
		}
		if res.Encoded[1].Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Encoded[1].Frames)
		}
		if res.Encoded[2].Frames != 240 {
			t.Error(in.Fname, " Mismatched frame count: expected 240 got ", res.Encoded[2].Frames)
		}
	}
	tc.StopTranscoder()
}