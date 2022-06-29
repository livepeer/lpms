package ffmpeg

import (
	"fmt"
	"io/ioutil"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAPI_SkippedSegment(t *testing.T) {
	// Ensure that things are still OK even if there are gaps in between segments
	// We should see a perf slowdown if the filters are in the wrong order
	// (fps before scale).
	// Would be nice to measure this somehow, eg counting number of flushed frames.

	// Really should refactor this test to increase commonality with other
	// tests that also check things like SSIM, MD5 hashes, etc...
	// See TestNvidia_API_MixedOutput / TestTranscoder_EncoderOpts / TestTranscoder_StreamCopy
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	err := RTMPToHLS("../transcoder/test.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}

	profile := P144p30fps16x9
	profile.Framerate = 123
	idx := []int{0, 3}
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	for _, i := range idx {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/out_%d.ts", dir, i)}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/%d.md5", dir, i),
			AudioEncoder: ComponentOptions{Name: "drop"},
			VideoEncoder: ComponentOptions{Name: "copy"},
			Muxer:        ComponentOptions{Name: "md5"},
		}, {
			Oname:        fmt.Sprintf("%s/sw_%d.ts", dir, i),
			Profile:      profile,
			AudioEncoder: ComponentOptions{Name: "copy"},
		}}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res.Decoded.Frames != 120 {
			t.Error("Did not get decoded frames", res.Decoded.Frames)
		}
		if res.Encoded[1].Frames != 246 {
			t.Error("Did not get encoded frames ", res.Encoded[1].Frames)
		}
	}
	cmd := `
    function check {

      # Check md5sum for stream copy / drop
      ffmpeg -loglevel warning -i out_$1.ts -an -c:v copy -f md5 ffmpeg_$1.md5
      diff -u $1.md5 ffmpeg_$1.md5

      # muxdelay removes the 1.4 sec mpegts offset, copyts passes ts through
      ffmpeg -loglevel warning -i out_$1.ts -c:a aac -ar 44100 -ac 2 \
        -vf fps=123,scale=w=256:h=144 -c:v libx264 -muxdelay 0 -copyts ffmpeg_sw_$1.ts

      # sanity check ffmpeg frame count against ours
      ffprobe -count_frames -show_streams -select_streams v ffmpeg_sw_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v sw_$1.ts | grep nb_read_frames=246

   # check image quality
    ffmpeg -loglevel warning -i sw_$1.ts -i ffmpeg_sw_$1.ts \
      -lavfi "[0:v][1:v]ssim=sw_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' sw_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    # Really should check relevant audio as well...
    }


    check 0
    check 3
  `
	run(cmd)

}

func TestTranscoderAPI_InvalidFile(t *testing.T) {
	// Test the following file open results on input: fail, success, fail, success

	tc := NewTranscoder()
	defer tc.StopTranscoder()
	in := &TranscodeOptionsIn{}
	out := []TranscodeOptions{{
		Oname:        "-",
		AudioEncoder: ComponentOptions{Name: "copy"},
		VideoEncoder: ComponentOptions{Name: "drop"},
		Muxer:        ComponentOptions{Name: "null"},
	}}

	// fail # 1
	in.Fname = "none"
	_, err := tc.Transcode(in, out)
	if err == nil || err.Error() != "TranscoderInvalidVideo" {
		// Early codec check didn't find video in missing input file so we get `TranscoderInvalidVideo`
		//  instead of `No such file or directory`
		t.Error("Expected 'TranscoderInvalidVideo', got ", err)
	}

	// success # 1
	in.Fname = "../transcoder/test.ts"
	_, err = tc.Transcode(in, out)
	if err != nil {
		t.Error(err)
	}

	// fail # 2
	in.Fname = "none"
	_, err = tc.Transcode(in, out)
	if err == nil || err.Error() != "No such file or directory" {
		t.Error("Expected 'No such file or directory', got ", err)
	}

	// success # 2
	in.Fname = "../transcoder/test.ts"
	_, err = tc.Transcode(in, out)
	if err != nil {
		t.Error(err)
	}

	// Now check invalid output filename
	out[0].Muxer = ComponentOptions{Name: "md5"}
	out[0].Oname = "/not/really/anywhere"
	_, err = tc.Transcode(in, out)
	if err == nil {
		t.Error(err)
	}

}

func TestTranscoderAPI_Stopped(t *testing.T) {

	// Test stopped transcoder
	tc := NewTranscoder()
	tc.StopTranscoder()
	in := &TranscodeOptionsIn{}
	_, err := tc.Transcode(in, nil)
	if err != ErrTranscoderStp {
		t.Errorf("Unexpected error; wanted %v but got %v", ErrTranscoderStp, err)
	}

	// test somehow munged transcoder handle
	tc2 := NewTranscoder()
	tc2.handle = nil // technically this leaks memory ... OK for test
	_, err = tc2.Transcode(in, nil)
	if err != ErrTranscoderStp {
		t.Errorf("Unexpected error; wanted %v but got %v", ErrTranscoderStp, err)
	}
}

func TestTranscoderAPI_TooManyOutputs(t *testing.T) {

	out := make([]TranscodeOptions, 11)
	for i := range out {
		out[i].VideoEncoder = ComponentOptions{Name: "drop"}
	}
	// We need input file with video stream to pass early format check
	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	tc := NewTranscoder()
	_, err := tc.Transcode(in, out)
	if err == nil || err.Error() != "Too many outputs" {
		t.Error("Expected 'Too many outputs', got ", err)
	}
}

func countEncodedFrames(t *testing.T, accel Acceleration) {
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
	defer tc.StopTranscoder()

	// Test encoding
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:  fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel:  accel,
			Device: "0",
		}
		p60fps := P144p30fps16x9
		p60fps.Framerate = 60
		p120fps := P144p30fps16x9
		p120fps.Framerate = 120
		passthru := P144p30fps16x9
		passthru.Framerate = 0
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out_30fps_%d.ts", dir, i),
			Profile: P144p30fps16x9,
			Accel:   accel,
		}, {
			Oname:   fmt.Sprintf("%s/out_60fps_%d.ts", dir, i),
			Profile: p60fps,
			Accel:   accel,
		}, {
			Oname:   fmt.Sprintf("%s/out_120fps_%d.ts", dir, i),
			Profile: p120fps,
			Accel:   accel,
		}, {
			Oname:   fmt.Sprintf("%s/out_passthru_%d.ts", dir, i),
			Profile: passthru,
			Accel:   accel,
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
		if res.Encoded[3].Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 120 got ", res.Encoded[3].Frames)
		}
	}

	// Check timestamps of the results we just got.
	// A bit brute force but another layer of checking
	// First output the expected PTS - first and last 3 frames, per segment
	//  (hardcoded below)
	// Second calculate the same set of PTS for transcoded results
	// Then take the diff of the two. Should match.

	// Write expected PTS to a file for diff'ing
	//  (Calculated by running the same routine below)
	cmd = `
  cat << EOF > expected_pts.out
==> out_120fps_0.ts.pts <==
pkt_pts=129000
pkt_pts=129750
pkt_pts=130500
pkt_pts=306750
pkt_pts=307500
pkt_pts=308250

==> out_120fps_1.ts.pts <==
pkt_pts=309000
pkt_pts=309750
pkt_pts=310500
pkt_pts=486750
pkt_pts=487500
pkt_pts=488250

==> out_120fps_2.ts.pts <==
pkt_pts=489000
pkt_pts=489750
pkt_pts=490500
pkt_pts=666750
pkt_pts=667500
pkt_pts=668250

==> out_120fps_3.ts.pts <==
pkt_pts=669000
pkt_pts=669750
pkt_pts=670500
pkt_pts=846750
pkt_pts=847500
pkt_pts=848250

==> out_30fps_0.ts.pts <==
pkt_pts=132000
pkt_pts=135000
pkt_pts=138000
pkt_pts=303000
pkt_pts=306000
pkt_pts=309000

==> out_30fps_1.ts.pts <==
pkt_pts=309000
pkt_pts=312000
pkt_pts=315000
pkt_pts=480000
pkt_pts=483000
pkt_pts=486000

==> out_30fps_2.ts.pts <==
pkt_pts=489000
pkt_pts=492000
pkt_pts=495000
pkt_pts=660000
pkt_pts=663000
pkt_pts=666000

==> out_30fps_3.ts.pts <==
pkt_pts=669000
pkt_pts=672000
pkt_pts=675000
pkt_pts=840000
pkt_pts=843000
pkt_pts=846000

==> out_60fps_0.ts.pts <==
pkt_pts=129000
pkt_pts=130500
pkt_pts=132000
pkt_pts=304500
pkt_pts=306000
pkt_pts=307500

==> out_60fps_1.ts.pts <==
pkt_pts=309000
pkt_pts=310500
pkt_pts=312000
pkt_pts=484500
pkt_pts=486000
pkt_pts=487500

==> out_60fps_2.ts.pts <==
pkt_pts=489000
pkt_pts=490500
pkt_pts=492000
pkt_pts=664500
pkt_pts=666000
pkt_pts=667500

==> out_60fps_3.ts.pts <==
pkt_pts=669000
pkt_pts=670500
pkt_pts=672000
pkt_pts=844500
pkt_pts=846000
pkt_pts=847500

==> out_passthru_0.ts.pts <==
pkt_pts=128970
pkt_pts=130500
pkt_pts=131940
pkt_pts=304470
pkt_pts=305910
pkt_pts=307440

==> out_passthru_1.ts.pts <==
pkt_pts=308970
pkt_pts=310500
pkt_pts=311940
pkt_pts=484470
pkt_pts=485910
pkt_pts=487440

==> out_passthru_2.ts.pts <==
pkt_pts=488970
pkt_pts=490410
pkt_pts=491940
pkt_pts=664470
pkt_pts=665910
pkt_pts=667440

==> out_passthru_3.ts.pts <==
pkt_pts=668970
pkt_pts=670500
pkt_pts=671940
pkt_pts=844470
pkt_pts=845910
pkt_pts=847440
EOF
  `
	run(cmd)

	// Calculate first and last 3 frame PTS for each output, and compare
	cmd = `
    # First and last 3 frame PTS for each output
    FILES=out_*.ts

    for f in  $FILES
    do
      ffprobe -loglevel warning -select_streams v -show_frames $f | grep pkt_pts= | head -3 > $f.pts
      ffprobe -loglevel warning -select_streams v -show_frames $f | grep pkt_pts= | tail -3 >> $f.pts
    done
    tail -n +1 out_*.ts.pts > transcoded_pts.out

    # Do the comparison!
    diff -u expected_pts.out transcoded_pts.out
  `
	run(cmd)

}

func TestTranscoderAPI_CountEncodedFrames(t *testing.T) {
	countEncodedFrames(t, Software)
}

func TestTranscoder_API_AlternatingTimestamps(t *testing.T) {
	// Really should refactor this test to increase commonality with other
	// tests that also check things like SSIM, MD5 hashes, etc...
	// See TestNvidia_API_MixedOutput / TestTranscoder_EncoderOpts / TestTranscoder_StreamCopy / TestNvidia_API_AlternatingTimestamps
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	err := RTMPToHLS("../transcoder/test.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}

	profile := P144p30fps16x9
	profile.Framerate = 123
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	idx := []int{1, 0, 3, 2}
	for _, i := range idx {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/out_%d.ts", dir, i)}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/%d.md5", dir, i),
			AudioEncoder: ComponentOptions{Name: "drop"},
			VideoEncoder: ComponentOptions{Name: "copy"},
			Muxer:        ComponentOptions{Name: "md5"},
		}, {
			Oname:        fmt.Sprintf("%s/sw_%d.ts", dir, i),
			Profile:      profile,
			AudioEncoder: ComponentOptions{Name: "copy"},
		}, {
			Oname:   fmt.Sprintf("%s/sw_audio_encode_%d.ts", dir, i),
			Profile: profile,
		}}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res != nil {
			if res.Decoded.Frames != 120 {
				t.Error("Did not get decoded frames", res.Decoded.Frames)
			}
			if res.Encoded[1].Frames != res.Encoded[2].Frames {
				t.Error("Mismatched frame count for hw/nv")
			}
		}
	}
	cmd := `
    function check {

      # Check md5sum for stream copy / drop
      ffmpeg -loglevel warning -i out_$1.ts -an -c:v copy -f md5 ffmpeg_$1.md5
      diff -u $1.md5 ffmpeg_$1.md5

      ffmpeg -loglevel warning -i out_$1.ts -c:a aac -ar 44100 -ac 2 \
        -vf fps=123,scale=w=256:h=144 -c:v libx264 -muxdelay 0 -copyts ffmpeg_sw_$1.ts

      # sanity check ffmpeg frame count against ours
      ffprobe -count_frames -show_streams -select_streams v ffmpeg_sw_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v sw_$1.ts | grep nb_read_frames=246
      ffprobe -count_frames -show_streams -select_streams v sw_audio_encode_$1.ts | grep nb_read_frames=246

    # check image quality
    ffmpeg -loglevel warning -i sw_$1.ts -i ffmpeg_sw_$1.ts \
      -lavfi "[0:v][1:v]ssim=sw_stats_$1.log" -f null -
    grep -Po 'All:\K\d+.\d+' sw_stats_$1.log | \
      awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'

    # Really should check relevant audio as well...
    }


    # re-enable for seg 0 and 1 when alternating timestamps can be handled
    check 0
    check 1
    check 2
    check 3
  `
	run(cmd)
}

// test short segments
func shortSegments(t *testing.T, accel Acceleration, fc int) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # generate segments with #fc frames
    cp "$1/../transcoder/test.ts" .
	frame_count=%d

    ffmpeg -loglevel warning -ss 0 -i test.ts -c copy -frames:v $frame_count -copyts short0.ts
    ffmpeg -loglevel warning -ss 2 -i test.ts -c copy -frames:v $frame_count -copyts short1.ts
    ffmpeg -loglevel warning -ss 4 -i test.ts -c copy -frames:v $frame_count -copyts short2.ts
    ffmpeg -loglevel warning -ss 6 -i test.ts -c copy -frames:v $frame_count -copyts short3.ts

    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short0.ts | grep nb_read_frames=$frame_count
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short1.ts | grep nb_read_frames=$frame_count
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short2.ts | grep nb_read_frames=$frame_count
    ffprobe -loglevel warning -count_frames -show_streams -select_streams v short3.ts | grep nb_read_frames=$frame_count
  `
	run(fmt.Sprintf(cmd, fc))

	// Test if decoding/encoding expected number of frames
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		oname := fmt.Sprintf("%s/out%d.ts", dir, i)
		t.Log("fname ", fname)
		in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
		out := []TranscodeOptions{{Oname: oname, Profile: P144p30fps16x9, Accel: accel}}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if fc != res.Decoded.Frames {
			t.Error("Did not decode expected number of frames: ", res.Decoded.Frames)
		}
		if res.Encoded[0].Frames == 0 {
			// TODO not sure what should be a reasonable number here
			t.Error("Did not encode any frames: ", res.Encoded[0].Frames)
		}
	}

	// test standalone stream copy
	tc.StopTranscoder()
	tc = NewTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		oname := fmt.Sprintf("%s/vcopy%d.ts", dir, i)
		in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
		out := []TranscodeOptions{
			{
				Oname: oname,
				VideoEncoder: ComponentOptions{Name: "copy", Opts: map[string]string{
					"mpegts_flags": "resend_headers,initial_discontinuity",
				}},
				Accel: accel,
			},
		}
		res, err := tc.Transcode(in, out)
		require.NoError(t, err)
		if res.Encoded[0].Frames != 0 {
			t.Error("Unexpected frame counts from stream copy")
			t.Error(res)
		}
		cmd = `
        # extract video track, compare md5sums
		i=%d
        ffmpeg -i short$i.ts -an -c:v copy -f md5 short$i.md5
        ffmpeg -i vcopy$i.ts -an -c:v copy -f md5 vcopy$i.md5
        diff -u short$i.md5 vcopy$i.md5
        `
		run(fmt.Sprintf(cmd, i))
	}

	// test standalone stream drop
	tc.StopTranscoder()
	tc = NewTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		oname := fmt.Sprintf("%s/vdrop%d.ts", dir, i)
		// Normal case : drop only video
		in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
		out := []TranscodeOptions{
			{
				Oname:        oname,
				VideoEncoder: ComponentOptions{Name: "drop"},
				Accel:        accel,
			},
		}
		res, err := tc.Transcode(in, out)
		require.NoError(t, err)
		if res.Decoded.Frames != 0 || res.Encoded[0].Frames != 0 {
			t.Error("Unexpected count of decoded frames ", res.Decoded.Frames, res.Decoded.Pixels)
		}

	}

	// test framerate passthrough
	tc.StopTranscoder()
	tc = NewTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		oname := fmt.Sprintf("%s/vpassthru%d.ts", dir, i)
		out := []TranscodeOptions{{Profile: P144p30fps16x9, Accel: accel}}
		out[0].Profile.Framerate = 0 // Passthrough!

		out[0].Oname = oname
		in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error("Could not transcode: ", err)
		}
		// verify that output frame count is same as input frame count
		if res.Decoded.Frames != fc || res.Encoded[0].Frames != fc {
			t.Error("Did not get expected frame count; got ", res.Encoded[0].Frames)
		}
	}

	// test low fps (3) to low fps (1)
	tc.StopTranscoder()
	tc = NewTranscoder()

	cmd = `
	frame_count=%d
	# convert segment to 3fps and trim it to #fc frames
	ffmpeg -loglevel warning -i test.ts -vf fps=3/1 -c:v libx264 -c:a copy -frames:v $frame_count short3fps.ts

	# sanity check
	ffprobe -loglevel warning -show_streams short3fps.ts | grep r_frame_rate=3/1
	ffprobe -loglevel warning -count_frames -show_streams -select_streams v short3fps.ts | grep nb_read_frames=$frame_count
  `
	run(fmt.Sprintf(cmd, fc))

	fname := fmt.Sprintf("%s/short3fps.ts", dir)
	in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
	out := []TranscodeOptions{{Oname: dir + "/out1fps.ts", Profile: P144p30fps16x9, Accel: accel}}
	out[0].Profile.Framerate = 1 // Force 1fps
	res, err := tc.Transcode(in, out)
	if err != nil {
		t.Error(err)
	}
	if fc != res.Decoded.Frames {
		t.Error("Did not decode expected number of frames: ", res.Decoded.Frames)
	}
	if res.Encoded[0].Frames == 0 {
		t.Error("Did not encode any frames: ", res.Encoded[0].Frames)
	}

	// test a bunch of weird cases together
	tc.StopTranscoder()
	tc = NewTranscoder()
	profile_low_fps := P144p30fps16x9
	profile_low_fps.Framerate = uint(fc) // use the input frame count as the output fps, why not
	profile_passthrough_fps := P144p30fps16x9
	profile_passthrough_fps.Framerate = 0
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf("%s/short%d.ts", dir, i)
		in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
		out := []TranscodeOptions{
			{
				Oname:        fmt.Sprintf("%s/lowfps%d.ts", dir, i),
				Profile:      profile_low_fps,
				AudioEncoder: ComponentOptions{Name: "copy"},
				Accel:        accel,
			},
			{
				Oname:        fmt.Sprintf("%s/copyall%d.ts", dir, i),
				VideoEncoder: ComponentOptions{Name: "copy"},
				AudioEncoder: ComponentOptions{Name: "drop"},
				Accel:        accel,
			},
			{
				Oname:   fmt.Sprintf("%s/passthru%d.ts", dir, i),
				Profile: profile_passthrough_fps,
				Accel:   accel,
			},
		}
		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res.Encoded[0].Frames == 0 || res.Encoded[1].Frames != 0 || res.Encoded[2].Frames != fc {
			t.Error("Unexpected frame counts from short segment copy-drop-passthrough case")
			t.Errorf("res: %+v", *res)
		}
	}

}

func TestTranscoder_ShortSegments(t *testing.T) {
	shortSegments(t, Software, 1)
	shortSegments(t, Software, 2)
	shortSegments(t, Software, 3)
	shortSegments(t, Software, 5)
	shortSegments(t, Software, 6)
	shortSegments(t, Software, 10)
}

func fractionalFPS(t *testing.T, accel Acceleration) {
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
	defer tc.StopTranscoder()

	// Test encoding
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname:  fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel:  accel,
			Device: "0",
		}
		// 23.98 fps
		p24fps := P144p30fps16x9
		p24fps.Framerate = 24 * 1000
		p24fps.FramerateDen = 1001
		// 29.97 fps
		p30fps := P144p30fps16x9
		p30fps.Framerate = 30 * 1000
		p30fps.FramerateDen = 1001
		// 59.94 fps
		p60fps := P144p30fps16x9
		p60fps.Framerate = 60 * 1000
		p60fps.FramerateDen = 1001
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out_23.98fps_%d.ts", dir, i),
			Profile: p24fps,
			Accel:   accel,
		}, {
			Oname:   fmt.Sprintf("%s/out_29.97fps_%d.ts", dir, i),
			Profile: p30fps,
			Accel:   accel,
		}, {
			Oname:   fmt.Sprintf("%s/out_59.94fps_%d.ts", dir, i),
			Profile: p60fps,
			Accel:   accel,
		}}

		res, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		if res.Encoded[0].Frames != 47 && res.Encoded[0].Frames != 48 {
			t.Error(in.Fname, " Mismatched frame count: expected 47 or 48 got ", res.Encoded[0].Frames)
		}
		if res.Encoded[1].Frames != 59 && res.Encoded[1].Frames != 60 {
			t.Error(in.Fname, " Mismatched frame count: expected 59 or 60 got ", res.Encoded[1].Frames)
		}
		if res.Encoded[2].Frames != 119 && res.Encoded[2].Frames != 120 {
			t.Error(in.Fname, " Mismatched frame count: expected 119 or 120 got ", res.Encoded[2].Frames)
		}

		cmd = `
		# check output FPS match the expected values
		i=%d
		ffprobe -loglevel warning -select_streams v -count_frames -show_streams out_23.98fps_$i.ts | grep r_frame_rate=24000/1001
		ffprobe -loglevel warning -select_streams v -count_frames -show_streams out_29.97fps_$i.ts | grep r_frame_rate=30000/1001
		ffprobe -loglevel warning -select_streams v -count_frames -show_streams out_59.94fps_$i.ts | grep r_frame_rate=60000/1001
	    `
		run(fmt.Sprintf(cmd, i))
	}

}

func TestTranscoder_FractionalFPS(t *testing.T) {
	fractionalFPS(t, Software)
}

func consecutiveMP4s(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
  cp "$1"/../transcoder/test.ts .
  ffmpeg -i test.ts -c copy -f segment test%d.mp4
  ffmpeg -i test.ts -c copy -f segment test%d.ts

  # manually convert ts to mp4 just to sanity check
  ffmpeg -i test0.ts -c copy -copyts -f mp4 test0.tsmp4
  ffmpeg -i test1.ts -c copy -copyts -f mp4 test1.tsmp4
  ffmpeg -i test2.ts -c copy -copyts -f mp4 test2.tsmp4
  ffmpeg -i test3.ts -c copy -copyts -f mp4 test3.tsmp4
`
	run(cmd)

	runTranscode := func(inExt, outExt string) {
		tc := NewTranscoder()
		defer tc.StopTranscoder()
		for i := 0; i < 4; i++ {
			fname := fmt.Sprintf("%s/test%d.%s", dir, i, inExt)
			oname := fmt.Sprintf("%s/%s_out_%d.%s", dir, inExt, i, outExt)
			in := &TranscodeOptionsIn{Fname: fname, Accel: accel}
			out := []TranscodeOptions{{Oname: oname, Profile: P240p30fps16x9, Accel: accel}}
			res, err := tc.Transcode(in, out)
			if err != nil {
				t.Error("Unexpected error ", err)
				continue
			}
			if res.Decoded.Frames != 120 || res.Encoded[0].Frames != 60 {
				t.Error("Unexpected results ", res)
			}
		}
	}

	inExts := []string{"ts", "mp4", "tsmp4"}
	outExts := []string{"ts", "mp4"}
	for _, inExt := range inExts {
		for _, outExt := range outExts {
			runTranscode(inExt, outExt)
		}
	}
}

func TestAPI_ConsecutiveMP4s(t *testing.T) {
	consecutiveMP4s(t, Software)
}

func TestAPI_ConsecutiveMuxerOpts(t *testing.T) {
	consecutiveMuxerOpts(t, Software)
}

func consecutiveMuxerOpts(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    cp "$1"/../transcoder/test.ts .
    ffmpeg -i test.ts -c copy -f segment seg%d.ts
    ls seg*.ts | wc -l | grep 4 # sanity check number of segments
  `
	run(cmd)

	// check with accel enabled
	tc := NewTranscoder()
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/seg%d.ts", dir, i), Accel: accel}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out%d.mp4", dir, i),
			Accel:   accel,
			Profile: P144p30fps16x9,
			Muxer: ComponentOptions{Opts: map[string]string{
				"brand": fmt.Sprintf("hi-%d", i)}},
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error("Unexpected error ", err)
		}
	}
	tc.StopTranscoder()

	cmd = `
    ffprobe -show_format out0.mp4 | grep brand=hi-0
    ffprobe -show_format out1.mp4 | grep brand=hi-1
    ffprobe -show_format out2.mp4 | grep brand=hi-2
    ffprobe -show_format out3.mp4 | grep brand=hi-3
  `
	run(cmd)

	// sanity check with non-encoding copy
	tc = NewTranscoder()
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/seg%d.ts", dir, i), Accel: Nvidia}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/copy%d.mp4", dir, i),
			VideoEncoder: ComponentOptions{Name: "copy"},
			Muxer: ComponentOptions{Opts: map[string]string{
				"brand": fmt.Sprintf("lo-%d", i)}},
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error("Unexpected error ", err)
		}
	}
	tc.StopTranscoder()

	cmd = `
    ffprobe -show_format copy0.mp4 | grep brand=lo-0
    ffprobe -show_format copy1.mp4 | grep brand=lo-1
    ffprobe -show_format copy2.mp4 | grep brand=lo-2
    ffprobe -show_format copy3.mp4 | grep brand=lo-3
  `
	run(cmd)

}

func TestTranscoder_EncodingProfiles(t *testing.T) {
	encodingProfiles(t, Software)
}

func encodingProfiles(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
        cp "$1/../transcoder/test.ts" test.ts
    `
	run(cmd)

	// First, sanity check defaults
	for _, p := range VideoProfileLookup {
		if p.Profile != ProfileNone {
			t.Error("Default VideoProfile profile not set to ProfileNone")
		}
	}

	// Encode to all H264 profiles
	profilesMap := make(map[Profile]string)
	for v := range ProfileParameters {
		switch v {
		case ProfileNone:
		case ProfileH264Baseline:
			profilesMap[v] = "baseline"
		case ProfileH264Main:
			profilesMap[v] = "main"
		case ProfileH264High:
			profilesMap[v] = "high"
		case ProfileH264ConstrainedHigh:
			profilesMap[v] = "constrained_high"
		default:
			t.Error("Unhandled profile ", v)
		}
	}

	for codecProfile, profileString := range profilesMap {
		tc := NewTranscoder()
		profile := P144p30fps16x9
		profile.Profile = codecProfile
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test.ts", dir), Accel: accel}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out_%s.mp4", dir, profileString),
			Accel:   accel,
			Profile: profile,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error("Unexpected error ", err)
		}
		tc.StopTranscoder()
	}

	// Verify outputs have correct profile names
	cmd = `
		ffprobe -loglevel warning -show_streams out_baseline.mp4 | grep "profile=Constrained Baseline"
		ffprobe -loglevel warning -show_streams out_main.mp4 | grep "profile=Main"
		ffprobe -loglevel warning -show_streams out_high.mp4 | grep "profile=High"
		ffprobe -loglevel warning -show_streams out_constrained_high.mp4 | grep "profile=High"
	`
	run(cmd)

	// Verify the two constrained profiles (Baseline & Constrained High) do not have any b frames
	cmd = `
		ffprobe -loglevel warning -show_streams out_baseline.mp4 | grep has_b_frames=0
		ffprobe -loglevel warning -show_streams out_constrained_high.mp4 | grep has_b_frames=0
	`
	run(cmd)

	// Verify the other two profiles have B frames
	cmd = `
		ffprobe -loglevel warning -show_streams out_main.mp4 | grep has_b_frames=2
		ffprobe -loglevel warning -show_streams out_high.mp4 | grep has_b_frames=2
	`
	// Interlaced Input with Constrained Profiles
	cmd = `
		ffmpeg -i test.ts -vf "tinterlace=5" -c:v libx264 -flags +ilme+ildct -x264opts bff=1 -c:a copy test_interlaced.ts

		# Sanity check that new input has interlaced flag set
		ffprobe test_interlaced.ts -show_frames | head -n 100 | grep interlaced_frame=1
	`
	run(cmd)
	constrainedProfilesMap := map[Profile]string{
		ProfileH264Baseline:        "baseline",
		ProfileH264ConstrainedHigh: "constrained_high",
	}
	for codecProfile, profileString := range constrainedProfilesMap {
		tc := NewTranscoder()
		profile := P144p30fps16x9
		profile.Profile = codecProfile
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test_interlaced.ts", dir), Accel: accel}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out_interlaced_%s.ts", dir, profileString),
			Accel:   accel,
			Profile: profile,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error("Unexpected error ", err)
		}
		tc.StopTranscoder()
	}
	cmd = `
		ffprobe out_interlaced_baseline.ts -show_frames | head -n 100 | grep interlaced_frame=0
		ffprobe out_interlaced_constrained_high.ts -show_frames | head -n 100 | grep interlaced_frame=0
	`
	run(cmd)

	// Unknown profile
	tc := NewTranscoder()
	profile := P144p30fps16x9
	profile.Profile = 420 // incorrect profile
	in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test.ts", dir), Accel: accel}
	out := []TranscodeOptions{{
		Oname:   fmt.Sprintf("%s/out_dummy.mp4", dir),
		Accel:   accel,
		Profile: profile,
	}}
	_, err := tc.Transcode(in, out)
	if err != ErrTranscoderPrf {
		t.Errorf("Unexpected error; wanted %v but got %v", ErrTranscoderPrf, err)
	}
	tc.StopTranscoder()
}

func TestAPI_SetGOPs(t *testing.T) {
	setGops(t, Software)
}

func setGops(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `

        cp "$1/../transcoder/test.ts" .

        # ffmpeg sanity checks

        ffmpeg -loglevel warning -i test.ts -c copy -f hls out.m3u8
        function prepare {
            f=$1

            ffprobe -loglevel warning $f -select_streams v -show_packets | grep flags=K | wc -l | grep 1
            ffprobe -loglevel warning $f -select_streams v -show_packets | grep flags= | wc -l | grep 120
            ffmpeg -loglevel warning -i $f -force_key_frames 'expr:if(isnan(prev_forced_t),1,gte(t,prev_forced_t+0.3))' -an -c:v libx264 -s 100x100 forced_$f
        }

        prepare out0.ts
        prepare out1.ts
        prepare out2.ts
        prepare out3.ts

        # extremely low frame rate
        ffmpeg -loglevel warning -i test.ts -c:v libx264 -r 1 lowfps.ts
        ffprobe -loglevel warning lowfps.ts -select_streams v -show_packets | grep flags= | wc -l | grep 9
        ffprobe -loglevel warning lowfps.ts -select_streams v -show_packets | grep flags=K | wc -l | grep 1
    `
	run(cmd)

	p := P144p30fps16x9
	p.GOP = 300 * time.Millisecond
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf(dir+"/out%d.ts", i)
		oname := fmt.Sprintf(dir+"/lpms%d.ts", i)
		_, err := tc.Transcode(&TranscodeOptionsIn{Fname: fname, Accel: accel}, []TranscodeOptions{{Oname: oname, Accel: accel, Profile: p}})
		if err != nil {
			t.Error(err)
		}
	}

	// passthru fps tests
	tc2 := NewTranscoder() // mitigate out of order segment issue
	defer tc2.StopTranscoder()
	p.Framerate = 0
	for i := 0; i < 4; i++ {
		fname := fmt.Sprintf(dir+"/out%d.ts", i)
		oname := fmt.Sprintf(dir+"/passthrough%d.ts", i)
		_, err := tc2.Transcode(&TranscodeOptionsIn{Fname: fname, Accel: accel}, []TranscodeOptions{{Oname: oname, Accel: accel, Profile: p}})
		if err != nil {
			t.Error(err)
		}
	}

	// extremely low frame rate with passthru fps
	p.GOP = 2 * time.Second
	o1 := TranscodeOptions{Oname: dir + "/lpms_lowfps.ts", Accel: accel, Profile: p}
	p2 := p
	p2.GOP = GOPIntraOnly // intra only
	o2 := TranscodeOptions{Oname: dir + "/lpms_intra.ts", Accel: accel, Profile: p2}
	p3 := p2
	p3.Framerate = 10
	o3 := TranscodeOptions{Oname: dir + "/lpms_intra_10fps.ts", Accel: accel, Profile: p3}
	_, err := Transcode3(&TranscodeOptionsIn{Fname: dir + "/lowfps.ts", Accel: accel}, []TranscodeOptions{o1, o2, o3})
	if err != nil {
		t.Error(err)
	}

	cmd = `
        function check {
            f=$1
            ffprobe -loglevel warning $f -select_streams v -show_packets | grep flags=K | wc -l | grep 7
        }

        # ensures we match ffmpeg with the same check
        check forced_out0.ts
        check forced_out1.ts
        check forced_out2.ts
        check forced_out3.ts

        check lpms0.ts
        check lpms1.ts
        check lpms2.ts
        check lpms3.ts

        check passthrough0.ts
        check passthrough1.ts
        check passthrough2.ts
        check passthrough3.ts

        # low framerate checks. sanity check number of packets vs keyframes
        ffprobe -loglevel warning lpms_lowfps.ts -select_streams v -show_packets | grep flags= | wc -l | grep 9
        ffprobe -loglevel warning lpms_lowfps.ts -select_streams v -show_packets | grep flags=K | wc -l | grep 5

        # intra checks with passthrough fps.
        # sanity check number of packets vs keyframes
        ffprobe -loglevel warning lpms_intra.ts -select_streams v -show_packets| grep flags= | wc -l | grep 9
        ffprobe -loglevel warning lpms_intra.ts -select_streams v -show_packets|grep flags=K | wc -l | grep 9

        # intra checks with fixed fps.
        # sanity check number of packets vs keyframes
        ffprobe -loglevel warning lpms_intra_10fps.ts -select_streams v -show_packets | grep flags= | wc -l | grep 90
        ffprobe -loglevel warning lpms_intra_10fps.ts -select_streams v -show_packets | grep flags=K | wc -l | grep 90
    `
	run(cmd)

	// check invalid gop lengths
	p.GOP = GOPInvalid
	_, err = tc.Transcode(&TranscodeOptionsIn{Fname: "asdf"}, []TranscodeOptions{{Oname: "qwerty", Profile: p}})
	if err == nil || err != ErrTranscoderGOP {
		t.Error("Did not expected error ", err)
	}
}

func TestTranscoder_SkippedFrames(t *testing.T) {
	// Reproducing #197
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
    cp "$1/../transcoder/test.ts" .
    ffmpeg -loglevel warning -i test.ts -c:a copy -vf fps=30 -c:v libx264 -muxdelay 0 test30.ts
    ffmpeg -loglevel warning -i test30.ts -vf select='between(n\,0\,49)' -c:a copy -c:v libx264 -muxdelay 0 -copyts source-0.ts
    ffmpeg -loglevel warning -i test30.ts -vf select='between(n\,60\,62)' -c:a copy -c:v libx264 -muxdelay 0 -copyts source-1.ts
    ffmpeg -loglevel warning -i test30.ts -vf select='between(n\,120\,179)' -c:a copy -c:v libx264 -muxdelay 0 -copyts source-2.ts
    for i in source-*.ts
    do
      ffprobe -show_streams -select_streams v $i | grep start_time >> source.txt
    done
  `
	run(cmd)

	tc := NewTranscoder()
	defer tc.StopTranscoder()

	prof := P144p30fps16x9
	prof.Framerate = 45
	// Test encoding
	for i := 0; i < 3; i++ {
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/source-%d.ts", dir, i),
			Accel: Software,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out-%d.ts", dir, i),
			Profile: prof,
			Accel:   Software,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
	}

	// Compare start times of original segments to the transcoded segments
	cmd = `
    for i in out-*.ts
    do
      ffprobe -show_streams -select_streams v $i | grep start_time >> out.txt
    done
    diff -u source.txt out.txt
  `
	run(cmd)
}

func audioOnlySegment(t *testing.T, accel Acceleration) {
	// Reproducing #203
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    # run segmenter and sanity check frame counts . Hardcode for now.
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -f hls test.m3u8
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test0.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test1.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test2.ts | grep nb_read_frames=120
    ffprobe -loglevel warning -select_streams v -count_frames -show_streams test3.ts | grep nb_read_frames=120

    # drop all video frames from seg2
    ffmpeg -loglevel warning -i test2.ts -c:a copy -c:v libx264 -vf "fps=fps=1/100000" test02.ts
    mv test02.ts test2.ts

    # verify no video frames in seg2
    ffprobe -loglevel warning -show_streams -select_streams v -count_frames test2.ts | grep nb_read_frames=N/A
  `
	run(cmd)

	// Test encoding with audio-only segment in between stream
	tc := NewTranscoder()
	prof := P144p30fps16x9
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out%d.ts", dir, i),
			Profile: prof,
			Accel:   accel,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
	}
	tc.StopTranscoder()

	// Test encoding with audio-only segment in start of stream
	tc = NewTranscoder()
	defer tc.StopTranscoder()
	for i := 2; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/test%d.ts", dir, i),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out2_%d.ts", dir, i),
			Profile: prof,
			Accel:   accel,
		}}
		_, err := tc.Transcode(in, out)
		if i == 2 && (err == nil || err.Error() != "TranscoderInvalidVideo") {
			t.Errorf("Expected to fail for audio-only segment but did not, instead got err=%v", err)
		} else if i != 2 && err != nil {
			t.Error(err)
		}
	}
}

func TestTranscoder_AudioOnly(t *testing.T) {
	audioOnlySegment(t, Software)
}

func outputFPS(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
	# convert test segment to 24fps and sanity check
	ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -vf fps=24 -c:a copy -c:v libx264 -f mpegts test24fps.ts
    ffprobe -loglevel warning -select_streams v -show_streams test24fps.ts | grep avg_frame_rate=24/1
  `
	run(cmd)

	tc := NewTranscoder()
	passthru := P144p30fps16x9
	passthru.Framerate = 0 // passthrough FPS
	in := &TranscodeOptionsIn{
		Fname: fmt.Sprintf("%s/test24fps.ts", dir),
		Accel: accel,
	}
	out := []TranscodeOptions{
		{
			Oname:   fmt.Sprintf("%s/out24fps.ts", dir),
			Profile: passthru,
			Accel:   accel,
		},
		{
			Oname:   fmt.Sprintf("%s/out30fps.ts", dir),
			Profile: P144p30fps16x9,
			Accel:   accel,
		},
	}
	_, err := tc.Transcode(in, out)
	if err != nil {
		t.Error(err)
	}
	tc.StopTranscoder()

	cmd = `
	# verify that the output fps reported in the muxed segment is correct for passthrough (fps=24)
    ffprobe -loglevel warning -select_streams v -show_streams out24fps.ts | grep avg_frame_rate=24/1
	# and non-passthrough (fps=30)
    ffprobe -loglevel warning -select_streams v -show_streams out30fps.ts | grep avg_frame_rate=30/1
  `
	run(cmd)
}

func TestTranscoder_OutputFPS(t *testing.T) {
	outputFPS(t, Software)
}

func TestTranscoderAPI_ClipInvalidConfig(t *testing.T) {
	run, dir := setupTest(t)
	cmd := `
		cp "$1"/../transcoder/test.ts .`
	run(cmd)
	defer os.RemoveAll(dir)
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test.ts", dir)}
	out := []TranscodeOptions{{
		Oname:        "-",
		VideoEncoder: ComponentOptions{Name: "drop"},
		From:         time.Second,
	}}
	_, err := tc.Transcode(in, out)
	if err == nil || err != ErrTranscoderClipConfig {
		t.Errorf("Expected '%s', got %v", ErrTranscoderClipConfig, err)
	}
	out[0].VideoEncoder.Name = "copy"
	_, err = tc.Transcode(in, out)
	if err == nil || err != ErrTranscoderClipConfig {
		t.Errorf("Expected '%s', got %v", ErrTranscoderClipConfig, err)
	}
	out[0].From = 0
	out[0].To = time.Second
	_, err = tc.Transcode(in, out)
	if err == nil || err != ErrTranscoderClipConfig {
		t.Errorf("Expected '%s', got %v", ErrTranscoderClipConfig, err)
	}
	out[0].VideoEncoder.Name = ""
	out[0].From = 10 * time.Second
	out[0].To = time.Second
	_, err = tc.Transcode(in, out)
	if err == nil || err != ErrTranscoderClipConfig {
		t.Errorf("Expected '%s', got %v", ErrTranscoderClipConfig, err)
	}
}

func noKeyframeSegment(t *testing.T, accel Acceleration) {
	// Reproducing #219
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
		cp "$1"/../data/kryp-*.ts .

    # verify no keyframes in kryp-2.ts but there in kryp-1.ts
    ffprobe -select_streams v -show_streams -show_packets kryp-1.ts | grep flags=K | wc -l | grep 1
    ffprobe -select_streams v -show_streams -show_packets kryp-2.ts | grep flags=K | wc -l | grep 0
  `
	run(cmd)

	prof := P144p30fps16x9
	for i := 1; i <= 2; i++ {
		tc := NewTranscoder()
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/kryp-%d.ts", dir, i),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out%d.ts", dir, i),
			Profile: prof,
			Accel:   accel,
		}}
		_, err := tc.Transcode(in, out)
		if i == 2 && (err == nil || err.Error() != "No keyframes in input") {
			t.Error("Expected to fail for no keyframe segment but did not")
		} else if i != 2 && err != nil {
			t.Error(err)
		}
		tc.StopTranscoder()
	}
}

func TestTranscoder_NoKeyframe(t *testing.T) {
	noKeyframeSegment(t, Software)
}

func nonMonotonicAudioSegment(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
    cp "$1"/../data/duplicate-audio-dts.ts .

    # verify dts non-monotonic audio frame in duplicate-audio-dts.ts
    ffprobe -select_streams a -show_streams -show_packets duplicate-audio-dts.ts | grep dts_time=98.127522 | wc -l | grep 2
  `
	run(cmd)

	tc := NewTranscoder()
	prof := P144p30fps16x9

	in := &TranscodeOptionsIn{
		Fname: fmt.Sprintf("%s/duplicate-audio-dts.ts", dir),
		Accel: accel,
	}
	out := []TranscodeOptions{{
		Oname:   fmt.Sprintf("%s/out-dts.ts", dir),
		Profile: prof,
		Accel:   accel,
	}}
	_, err := tc.Transcode(in, out)
	if err != nil {
		t.Error("Expected to succeed for a segment with non-monotonic audio frame but did not")
	}

	tc.StopTranscoder()
}
func TestTranscoder_NonMonotonicAudioSegment(t *testing.T) {
	nonMonotonicAudioSegment(t, Software)
}

func discontinuityPixelFormatSegment(t *testing.T, accel Acceleration) {

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
	cp "$1/../transcoder/test.ts" test.ts

	# generate yuv420p segments
    ffmpeg -loglevel warning -i test.ts -an -c:v copy -t 1 inyuv420p-1.mp4
	ffmpeg -loglevel warning -i test.ts -an -c:v copy -t 1 inyuv420p-3.mp4
    ffprobe -loglevel warning inyuv420p-1.mp4  -show_streams -select_streams v | grep pix_fmt=yuv420p

	# generate yuvj420p type
    ffmpeg -loglevel warning -i test.ts -an -c:v libx264 -pix_fmt yuvj420p -t 1 inyuv420p-2.mp4
    ffprobe -loglevel warning inyuv420p-2.mp4  -show_streams -select_streams v | grep pix_fmt=yuvj420p
	`
	run(cmd)

	tc := NewTranscoder()
	prof := P144p30fps16x9
	for i := 1; i <= 3; i++ {
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/inyuv420p-%d.mp4", dir, i),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out%d.ts", dir, i),
			Profile: prof,
			Accel:   accel,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
	}
	tc.StopTranscoder()
}

func TestTranscoder_DiscontinuityPixelFormat(t *testing.T) {
	discontinuityPixelFormatSegment(t, Software)
}

func compareVideo(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
	cp "$1/../transcoder/test.ts" test.ts
	`
	run(cmd)

	prof := P720p60fps16x9
	for i := 1; i <= 3; i++ {
		tc := NewTranscoder()
		if i == 3 {
			prof = P144p30fps16x9
		}
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/test.ts", dir),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Oname:   fmt.Sprintf("%s/out-%d.ts", dir, i),
			Profile: prof,
			Accel:   accel,
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
		tc.StopTranscoder()
	}

	res, err := CompareVideoByPath(dir+"/out-1.ts", dir+"/out-2.ts")
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareVideoByPath(dir+"/out-1.ts", dir+"/out-3.ts")
	if err != nil || res != false {
		t.Error(err)
	}
	res, err = CompareVideoByPath(dir+"/out-1.ts", dir+"/out-4.ts")
	if err == nil || res != false {
		t.Error(err)
	}

	//test ByBuffer function
	data1, err := ioutil.ReadFile(dir + "/out-1.ts")
	if err != nil {
		t.Error(err)
	}
	data2, err := ioutil.ReadFile(dir + "/out-2.ts")
	if err != nil {
		t.Error(err)
	}
	data3, err := ioutil.ReadFile(dir + "/out-3.ts")
	if err != nil {
		t.Error(err)
	}

	res, err = CompareVideoByBuffer(data1, data2)
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareVideoByBuffer(data1, data3)
	if err != nil || res != false {
		t.Error(err)
	}
}
func TestTranscoder_CompareVideo(t *testing.T) {
	compareVideo(t, Software)
}

func detectionFreq(t *testing.T, accel Acceleration, deviceid string) {
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

	InitFFmpeg()
	tc, err := NewTranscoderWithDetector(&DSceneAdultSoccer, deviceid)
	require.NotNil(t, tc, "look for `Failed to load native model` logs above")
	if err != nil {
		t.Error(err)
	} else {
		defer tc.StopTranscoder()
		// Test encoding with only seg0 and seg2 under detection
		prof := P144p30fps16x9
		for i := 0; i < 4; i++ {
			in := &TranscodeOptionsIn{
				Fname: fmt.Sprintf("%s/test%d.ts", dir, i),
				Accel: accel,
			}
			out := []TranscodeOptions{
				{
					Oname:   fmt.Sprintf("%s/out%d.ts", dir, i),
					Profile: prof,
					Accel:   accel,
				},
			}
			if i%2 == 0 {
				out = append(out, TranscodeOptions{
					Detector: &DSceneAdultSoccer,
					Accel:    accel,
				})
			}
			res, err := tc.Transcode(in, out)
			if err != nil {
				t.Error(err)
			}
			if i%2 == 0 && (len(res.Encoded) < 2 || res.Encoded[1].DetectData == nil) {
				t.Error("No detect data returned for detection profile")
			}
		}
	}
}

func TestTranscoder_DetectionFreq(t *testing.T) {
	detectionFreq(t, Software, "-1")
}
