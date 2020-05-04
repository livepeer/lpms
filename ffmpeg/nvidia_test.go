// +build nvidia

package ffmpeg

import (
	"fmt"
	"os"
	"testing"
)

func TestNvidia_Transcoding(t *testing.T) {
	// Various Nvidia GPU tests for encoding + decoding
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

	// hw dec, sw enc
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Software,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// sw dec, hw enc
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Software,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// hw enc + dec
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
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
		{
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

func TestNvidia_BadCodecs(t *testing.T) {
	// Following test case validates that the transcoder throws correct errors for unsupported codecs
	// Currently only H264 is supported

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	fname := dir + "/mpeg2.ts"
	oname := dir + "/out.ts"
	prof := P240p30fps16x9

	cmd := `
	cp "$1/../transcoder/test.ts" test.ts
		# Generate an input file that is not H264 (mpeg2) and sanity check
		ffmpeg -loglevel warning -i test.ts -an -c:v mpeg2video -t 1 mpeg2.ts
		ffprobe -loglevel warning mpeg2.ts -show_streams | grep codec_name=mpeg2video
	`
	run(cmd)

	// sanity check
	err := Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
		},
	})

	if err == nil || err.Error() != "Unsupported input codec" {
		t.Error(err)
	}

}

func TestNvidia_Pixfmts(t *testing.T) {

	// Following test case validates pixel format at the decoding end
	// Currently only YUV 4:2:0 is supported
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	prof := P240p30fps16x9

	cmd := `
	cp "$1/../transcoder/test.ts" test.ts
	# sanity check original input type is 420p
    ffmpeg -loglevel warning -i test.ts -an -c:v copy -t 1 in420p.mp4
    ffprobe -loglevel warning in420p.mp4  -show_streams -select_streams v | grep pix_fmt=yuv420p

    # generate unsupported 422p type
    ffmpeg -loglevel warning -i test.ts -an -c:v libx264 -pix_fmt yuv422p -t 1 in422p.mp4
    ffprobe -loglevel warning in422p.mp4  -show_streams -select_streams v | grep pix_fmt=yuv422p
	`
	run(cmd)

	// sanity check
	err := Transcode2(&TranscodeOptionsIn{
		Fname: dir + "/in420p.mp4",
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   dir + "/out420p.mp4",
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// check an input pixel format that is not GPU decodeable
	err = Transcode2(&TranscodeOptionsIn{
		Fname: dir + "/in422p.mp4",
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   dir + "/out422p.mp4",
			Profile: prof,
			Accel:   Software,
		},
	})
	if err == nil || err.Error() != "Unsupported input pixel format" {
		t.Error(err)
	}

	cmd = `
    # Check that 420p input produces 420p output for hw -> hw
    ffprobe -loglevel warning out420p.mp4  -show_streams -select_streams v | grep pix_fmt=yuv420p
  `
	run(cmd)

	// Check that yuvj420p is allowed (jpeg color range)
	// TODO check for color range truncation?
	cmd = `
    ffmpeg -loglevel warning -i test.ts -an -c:v libx264 -pix_fmt yuvj420p -t 1 inj420p.mp4
    ffprobe -loglevel warning inj420p.mp4  -show_streams -select_streams v | grep pix_fmt=yuvj420p
  `
	run(cmd)
	err = Transcode2(&TranscodeOptionsIn{
		Fname: dir + "/inj420p.mp4",
		Accel: Nvidia,
	}, []TranscodeOptions{{
		Oname:   dir + "/outj420p.mp4",
		Profile: prof,
		Accel:   Nvidia,
	},
	})
	if err != nil {
		t.Error(err)
	}

	return
	// Following test vaidates YUV 4:4:4 pixel format encoding which is only supported on limited set of devices for e.g. P100
	// https://developer.nvidia.com/video-encode-decode-gpu-support-matrix

	// check valid and invalid pixel formats
	cmd = `
    # generate semi-supported 444p type (encoding only)
    ffmpeg -loglevel warning -i test.ts -an -c:v libx264 -pix_fmt yuv444p -t 1 in444p.mp4
    ffprobe -loglevel warning in444p.mp4  -show_streams -select_streams v | grep pix_fmt=yuv444p
  `
	run(cmd)

	// 444p is encodeable but not decodeable; produces a different error
	// that is only caught at decode time. Attempt to detect decode bailout.
	err = Transcode2(&TranscodeOptionsIn{
		Fname: dir + "/in444p.mp4",
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   dir + "/out444p.mp4",
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err == nil || err.Error() != "Invalid data found when processing input" {
		t.Error(err)
	}

	// Software decode an and attempt to encode an invalid GPU pixfmt
	// This implicitly selects a supported pixfmt for output
	// The default Seems to be 444p for P100 but may be 420p for others
	err = Transcode2(&TranscodeOptionsIn{
		Fname: dir + "/in422p.mp4",
		Accel: Software,
	}, []TranscodeOptions{
		{
			Oname:   dir + "/out422_to_default.mp4",
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err != nil {
		t.Error(err)
	}

	cmd = `
	# 422p input (with sw decode) produces 444p on P100.
    # Cards that don't do 444p may do 420p instead (untested)
	ffprobe -loglevel warning out422_to_default.mp4  -show_streams -select_streams v | grep 'pix_fmt=\(yuv420p\|yuv444p\)'
	
    # 422p input (with sw decode) produces 444p on P100.
    # Cards that don't do 444p may do 420p instead (untested)
    ffprobe -loglevel warning out422_to_default.mp4  -show_streams -select_streams v | grep 'pix_fmt=\(yuv420p\|yuv444p\)'
  `
	run(cmd)
}

func TestNvidia_Transcoding_Multiple(t *testing.T) {

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
		{
			Oname: mkoname(0),
			// pass through resolution has different behavior;
			// basically bypasses the scale filter
			Profile: orig,
			Accel:   Nvidia,
		},
		{
			Oname:   mkoname(1),
			Profile: prof,
			Accel:   Nvidia,
		},
		{
			Oname:   mkoname(2),
			Profile: orig,
			Accel:   Software,
		},
		{
			Oname:   mkoname(3),
			Profile: prof,
			Accel:   Software,
		},
		// another gpu rendition for good measure?
		{
			Oname:   mkoname(4),
			Profile: prof,
			Accel:   Nvidia,
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
	test(Nvidia)
	test(Software)
}

func TestNvidia_Devices(t *testing.T) {

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

	// hw enc, sw dec
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  Nvidia,
		Device: device,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Software,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// sw dec, hw enc
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Software,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
			Device:  device,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// hw enc + dec
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  Nvidia,
		Device: device,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err != nil {
		t.Error(err)
	}

	// hw enc + hw dec, separate devices
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  Nvidia,
		Device: "0",
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
			Device:  "1",
		},
	})
	if err != ErrTranscoderInp {
		t.Error(err)
	}

	// invalid device for decoding
	err = Transcode2(&TranscodeOptionsIn{
		Fname:  fname,
		Accel:  Nvidia,
		Device: "9999",
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Software,
		},
	})
	if err == nil || err.Error() != "Unknown error occurred" {
		t.Error(fmt.Errorf(fmt.Sprintf("\nError being: '%v'\n", err)))
	}

	// invalid device for encoding
	err = Transcode2(&TranscodeOptionsIn{
		Fname: fname,
		Accel: Software,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
			Device:  "9999",
		},
	})
	if err == nil || err.Error() != "Unknown error occurred" {
		t.Error(fmt.Errorf(fmt.Sprintf("\nError being: '%v'\n", err)))
	}
}

func TestNvidia_DrainFilters(t *testing.T) {
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
		Accel: Nvidia,
	}, []TranscodeOptions{
		{
			Oname:   oname,
			Profile: prof,
			Accel:   Nvidia,
		},
	})
	if err != nil {
		t.Error(err)
	}

	cmd = `
    # sanity check with ffmpeg itself
    ffmpeg -loglevel warning -i test.ts -c:a copy -c:v libx264 -vf fps=100 -vsync 0 ffmpeg-out.ts
    ffprobe -loglevel warning -show_streams -select_streams v -count_frames ffmpeg-out.ts > ffmpeg,out
    grep nb_read_frames ffmpeg,out > ffmpeg-read-frames.out
    grep duration= ffmpeg,out > ffmpeg-duration.out

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

func TestNvidia_CountFrames(t *testing.T) {
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
			Accel:  Nvidia,
			Device: "3",
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

func TestNvidia_CountEncodedFrames(t *testing.T) {
	countEncodedFrames(t, Nvidia)
}

func TestNvidia_RepeatedSpecialOpts(t *testing.T) {

	_, dir := setupTest(t)

	err := RTMPToHLS("../transcoder/test.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}

	// At some point we forgot to set the muxer type in reopened outputs
	// This used to cause an error, so just check that it's resolved
	in := &TranscodeOptionsIn{Accel: Nvidia}
	out := []TranscodeOptions{{
		Oname:        "-",
		Profile:      P144p30fps16x9,
		VideoEncoder: ComponentOptions{Opts: map[string]string{"zerolatency": "1"}},
		Muxer:        ComponentOptions{Name: "null"},
		Accel:        Nvidia}}
	tc := NewTranscoder()
	for i := 0; i < 4; i++ {
		in.Fname = fmt.Sprintf("%s/out_%d.ts", dir, i)
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
	}
	tc.StopTranscoder()

	// ALso test when a repeated option fails ?? Special behaviour for this?
}

func TestNvidia_API_MixedOutput(t *testing.T) {
	run, dir := setupTest(t)
	err := RTMPToHLS("../transcoder/test.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}

	profile := P144p30fps16x9
	profile.Framerate = 123
	tc := NewTranscoder()
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/out_%d.ts", dir, i)}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/%d.md5", dir, i),
			AudioEncoder: ComponentOptions{Name: "drop"},
			VideoEncoder: ComponentOptions{Name: "copy"},
			Muxer:        ComponentOptions{Name: "md5"},
		}, {
			Oname:        fmt.Sprintf("%s/nv_%d.ts", dir, i),
			Profile:      profile,
			AudioEncoder: ComponentOptions{Name: "copy"},
			Accel:        Nvidia,
		}, {
			Oname:   fmt.Sprintf("%s/nv_audio_encode_%d.ts", dir, i),
			Profile: profile,
			Accel:   Nvidia,
		}, {
			Oname:   fmt.Sprintf("%s/sw_%d.ts", dir, i),
			Profile: profile,
		}}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error("Did not get decoded frames", res.Decoded.Frames)
		}
		if res.Encoded[1].Frames != res.Encoded[2].Frames {
			t.Error("Mismatched frame count for hw/nv")
		}
	}
	cmd := `
    function check {

      # Check md5sum for stream copy / drop
      ffmpeg -loglevel warning -i out_$1.ts -an -c:v copy -f md5 ffmpeg_$1.md5
      diff -u $1.md5 ffmpeg_$1.md5

      ffmpeg -loglevel warning -i out_$1.ts -c:a aac -ar 44100 -ac 2 \
        -vf hwupload_cuda,fps=123,scale_cuda=w=256:h=144 -c:v h264_nvenc \
        ffmpeg_nv_$1.ts

      # sanity check ffmpeg frame count against ours
      ffprobe -count_frames -show_streams -select_streams v ffmpeg_nv_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v nv_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v sw_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v nv_audio_encode_$1.ts | grep nb_read_frames=246

    # check image quality
    ffmpeg -loglevel warning -i nv_$1.ts -i ffmpeg_nv_$1.ts \
      -lavfi "[0:v][1:v]ssim=nv_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' nv_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    ffmpeg -loglevel warning -i sw_$1.ts -i ffmpeg_nv_$1.ts \
      -lavfi "[0:v][1:v]ssim=sw_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' sw_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    # Really should check relevant audio as well...

    }


    check 0
    check 1
    check 2
    check 3
  `
	run(cmd)
	tc.StopTranscoder()
}

func TestNvidia_API_AlternatingTimestamps(t *testing.T) {
	// Really should refactor this test to increase commonality with other
	// tests that also check things like SSIM, MD5 hashes, etc...
	// See TestNvidia_API_MixedOutput / TestTranscoder_EncoderOpts / TestTranscoder_StreamCopy
	run, dir := setupTest(t)
	err := RTMPToHLS("../transcoder/test.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}

	profile := P144p30fps16x9
	profile.Framerate = 123
	tc := NewTranscoder()
	idx := []int{1, 0, 3, 2}
	for _, i := range idx {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/out_%d.ts", dir, i)}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/%d.md5", dir, i),
			AudioEncoder: ComponentOptions{Name: "drop"},
			VideoEncoder: ComponentOptions{Name: "copy"},
			Muxer:        ComponentOptions{Name: "md5"},
		}, {
			Oname:        fmt.Sprintf("%s/nv_%d.ts", dir, i),
			Profile:      profile,
			AudioEncoder: ComponentOptions{Name: "copy"},
			Accel:        Nvidia,
		}, {
			Oname:   fmt.Sprintf("%s/nv_audio_encode_%d.ts", dir, i),
			Profile: profile,
			Accel:   Nvidia,
		}, {
			Oname:   fmt.Sprintf("%s/sw_%d.ts", dir, i),
			Profile: profile,
		}}
		res, err := tc.Transcode(in, out)
		if (i == 1 || i == 3) && err != nil {
			t.Error(err)
		}
		if i == 0 || i == 2 {
			if err == nil || err.Error() != "Segment out of order" {
				t.Error(err)
			}
			// Maybe one day we'll be able to run the rest of this test
			continue
		}
		if res.Decoded.Frames != 120 {
			t.Error("Did not get decoded frames", res.Decoded.Frames)
		}
		if res.Encoded[1].Frames != res.Encoded[2].Frames {
			t.Error("Mismatched frame count for hw/nv")
		}
	}
	cmd := `
    function check {

      # Check md5sum for stream copy / drop
      ffmpeg -loglevel warning -i out_$1.ts -an -c:v copy -f md5 ffmpeg_$1.md5
      diff -u $1.md5 ffmpeg_$1.md5

      ffmpeg -loglevel warning -i out_$1.ts -c:a aac -ar 44100 -ac 2 \
        -vf hwupload_cuda,fps=123,scale_cuda=w=256:h=144 -c:v h264_nvenc \
        ffmpeg_nv_$1.ts

      # sanity check ffmpeg frame count against ours
      ffprobe -count_frames -show_streams -select_streams v ffmpeg_nv_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v nv_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v sw_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v nv_audio_encode_$1.ts | grep nb_read_frames=246

    # check image quality
    ffmpeg -loglevel warning -i nv_$1.ts -i ffmpeg_nv_$1.ts \
      -lavfi "[0:v][1:v]ssim=nv_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' nv_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    ffmpeg -loglevel warning -i sw_$1.ts -i ffmpeg_nv_$1.ts \
      -lavfi "[0:v][1:v]ssim=sw_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' sw_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    # Really should check relevant audio as well...
    }


    # re-enable for seg 0 and 1 when alternating timestamps can be handled
    # check 0
    check 1
    # check 2
    check 3
  `
	run(cmd)
	tc.StopTranscoder()
}

func TestNvidia_ShortSegments(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # generate 1-frame segments
    cp "$1/../transcoder/test.ts" .

    ffmpeg -loglevel warning -ss 0 -i test.ts -c copy -frames:v 1 -copyts short0.ts
    ffmpeg -loglevel warning -ss 2 -i test.ts -c copy -frames:v 1 -copyts short1.ts
    ffmpeg -loglevel warning -ss 4 -i test.ts -c copy -frames:v 1 -copyts short2.ts
    ffmpeg -loglevel warning -ss 6 -i test.ts -c copy -frames:v 1 -copyts short3.ts

    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short0.ts | grep nb_read_frames=1
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short1.ts | grep nb_read_frames=1
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short2.ts | grep nb_read_frames=1
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short3.ts | grep nb_read_frames=1
  `
	run(cmd)

	tc := NewTranscoder()
	defer tc.StopTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		t.Log("fname ", fname)
		in := &TranscodeOptionsIn{Fname: fname, Accel: Nvidia}
		out := []TranscodeOptions{{Oname: dir + "/out.ts", Profile: P144p30fps16x9, Accel: Nvidia}}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if 1 != res.Decoded.Frames {
			t.Error("Did not decode expected number of frames: ", res.Decoded.Frames)
		}
		if 0 == res.Encoded[0].Frames {
			// not sure what should be a reasonable number here
			t.Error("Did not encode any frames: ", res.Encoded[0].Frames)
		}
	}

	// Also test:
	//  stream copy (both in conjunction with transcoding and standalone)
	//  stream dropping (both in conjunction with transcoding and standalone)
	//  framerate pass through
	//  transcode low frame rate to low frame rate
	//  other sanity checks for slightly higher frame numbers -
	//   try 2, 3, 5 frames in addition to the 1 frame test case above.
	// also test non-cuda, eg via api_test.go
	// parameterize test sequence for all the above?
}

// XXX test bframes or delayed frames
