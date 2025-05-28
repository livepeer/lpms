package ffmpeg

import (
	"bufio"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTest(t *testing.T) (func(cmd string) bool, string) {
	dir, err := ioutil.TempDir("", t.Name())
	if err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	InitFFmpeg() // hide some log noise

	// Executes the given bash script and checks the results.
	// The script is passed two arguments:
	// a tempdir and the current working directory.
	cmdFunc := func(cmd string) bool {
		cmd = "cd $0 && set -eux;\n" + cmd
		out, err := exec.Command("bash", "-c", cmd, dir, wd).CombinedOutput()
		if err != nil {
			t.Error(string(out[:]))
			return false
		}
		return true
	}
	return cmdFunc, dir
}

func TestSegmenter_DeleteSegments(t *testing.T) {
	// Ensure that old segments are deleted as they fall off the playlist

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// sanity check that segmented outputs > playlist length
	cmd := `
		# default test.ts is a bit short so make it a bit longer
		cp "$1/../transcoder/test.ts" test.ts
		ffmpeg -loglevel warning -i "concat:test.ts|test.ts|test.ts" -c copy long.ts
		ffmpeg -loglevel warning -i long.ts -c copy -f hls -hls_time 1 long.m3u8
		# ensure we have more segments than playlist length
		[ $(ls long*.ts | wc -l) -ge 6 ]
	`
	run(cmd)

	// actually do the segmentation
	err := RTMPToHLS(dir+"/long.ts", dir+"/out.m3u8", dir+"/out_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// check that segments have been deleted by counting output ts files
	cmd = `
		[ $(ls out_*.ts | wc -l) -eq 6 ]
	`
	run(cmd)
}

func TestSegmenter_StreamOrdering(t *testing.T) {
	// Ensure segmented output contains [video, audio] streams in that order
	// regardless of stream ordering in the input

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input that has a subtitle, audio and video stream, in that order
	cmd := `
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
	run(cmd)

	// actually do the segmentation
	err := RTMPToHLS(dir+"/test.mp4", dir+"/out.m3u8", dir+"/out_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// check stream ordering in output file. Should be video, then audio
	cmd = `
		[ $(ffprobe -loglevel warning -i out_0.ts -show_streams | grep index | wc -l) -eq 2 ]
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams v | grep index=0
		ffprobe -loglevel warning -i out_0.ts -show_streams -select_streams a | grep index=1
	`
	run(cmd)
}

func TestSegmenter_DropLatePackets(t *testing.T) {
	// Certain sources sometimes send packets with out-of-order FLV timestamps
	// (eg, ManyCam on Android when the phone can't keep up)
	// Ensure we drop these packets

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input with an out-of-order timestamp
	cmd := `
		# borrow segmenter test file, rewrite a timestamp
		cp "$1/../segmenter/test.flv" test.flv

		# Sanity check the last few timestamps are monotonic : 18867,18900,18933
		ffprobe -loglevel quiet -show_packets -select_streams v test.flv | grep dts= | tail -3 | tr '\n' ',' | grep dts=18867,dts=18900,dts=18933,

		# replace ts 18900 at position 2052736 with ts 18833 (0x4991 hex)
		printf '\x49\x91' | dd of=test.flv bs=1 seek=2052736 count=2 conv=notrunc
		# sanity check timestamps are now 18867,18833,18933
		ffprobe -loglevel quiet -show_packets -select_streams v test.flv | grep dts= | tail -3 | tr '\n' ',' | grep dts=18867,dts=18833,dts=18933,

		# sanity check number of frames
		ffprobe -loglevel quiet -count_packets -show_streams -select_streams v test.flv | grep nb_read_packets=569
	`
	run(cmd)

	err := RTMPToHLS(dir+"/test.flv", dir+"/out.m3u8", dir+"/out_%d.ts", "100", 0)
	if err != nil {
		t.Error(err)
	}

	// Now ensure things are as expected
	cmd = `
		# check monotonic timestamps (rescaled for the 90khz mpegts timebase)
		ffprobe -loglevel quiet -show_packets -select_streams v out_0.ts | grep dts= | tail -3 | tr '\n' ',' | grep dts=1694970,dts=1698030,dts=1703970,

		# check that we dropped the packet
		ffprobe -loglevel quiet -count_packets -show_streams -select_streams v out_0.ts | grep nb_read_packets=568
	`
	run(cmd)
}

func TestTranscoder_Resolution(t *testing.T) {
	runResolutionTests_H264(t, Software)
	// TODO test HEVC clamping
}

func runResolutionTests_H264(t *testing.T, accel Acceleration) {
	// Test clamping behavior of rescaler
	// and that aspect ratio is still maintained

	// TODO make it possible to run setupTest within sub-tests
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	tests := []struct {
		name string
		// input width and height
		input string
		// target resolution
		target string
		// expected width and height
		expected string
	}{{
		name:     "h > w",
		input:    "150x200",
		target:   "0x250",
		expected: "188x250",
	}, {
		name:     "h > w, rounded height",
		input:    "200x300",
		target:   "0x427",
		expected: "284x426",
	}, {
		name:     "h > w, target swapped",
		input:    "200x300",
		target:   "426x0",
		expected: "284x426",
	}, {
		name:     "h > w, w < min",
		input:    "123x456",
		target:   "0x426",
		expected: "146x542",
	}, {
		name:     "h > w, rounded width",
		input:    "200x300",
		target:   "0x428",
		expected: "286x428",
	}, {
		name:     "h > w, w < min and h < min",
		input:    "400x456",
		target:   "0x40",
		expected: "146x166", // will always hit min width here
	}, {
		name:     "w > h",
		input:    "456x123",
		target:   "426x0",
		expected: "426x114",
	}, {
		name:     "w > h, target swapped and rounded width",
		input:    "456x123",
		target:   "0x301",
		expected: "300x80",
	}, {
		name:     "w > h, w < min",
		input:    "456x400",
		target:   "100x0",
		expected: "146x128",
	}, {
		name:     "w > h, target swapped and h < min",
		input:    "500x100",
		target:   "0x200",
		expected: "250x50",
	}, {
		name:     "w > h, target swapped",
		input:    "456x120",
		target:   "0x400",
		expected: "400x106",
	}, {
		name:     "square",
		input:    "123x123",
		target:   "426x0",
		expected: "426x426",
	}}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// TODO optimize  by reusing inputs if possible
			cmd := fmt.Sprintf(`
				echo '%s'
				ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:a copy -c:v mpeg4 -s %s -t 1 test-%d.mp4
				ffprobe -hide_banner -show_entries stream=width,height -of csv=p=0:s=x test-%d.mp4 | grep %s
		`, tt.name, tt.input, i, i, tt.input)
			run(cmd)
			_, err := Transcode3(&TranscodeOptionsIn{
				Fname: fmt.Sprintf("%s/test-%d.mp4", dir, i),
			}, []TranscodeOptions{{
				Oname:   fmt.Sprintf("%s/out-test-%d.mp4", dir, i),
				Profile: VideoProfile{Resolution: tt.target, Bitrate: "50k"},
				Accel:   accel,
			}})
			assert.Nil(t, err)
			cmd = fmt.Sprintf(`
					echo '%s'
					ffprobe -hide_banner -show_entries stream=width,height -of csv=p=0:s=x out-test-%d.mp4 | grep %s`, tt.name, i, tt.expected)
			run(cmd)
		})
	}
	// TODO set / check sar/dar values?
}

func TestTranscoder_SampleRate(t *testing.T) {

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Craft an input with 48khz audio
	cmd := `
		# borrow the test.ts from the transcoder dir, output with 48khz audio
		ffmpeg -loglevel warning -i "$1/../transcoder/test.ts" -c:v copy -af 'aformat=sample_fmts=fltp:channel_layouts=stereo:sample_rates=48000' -c:a aac -t 1.1 test.ts

		# sanity check results to ensure preconditions
		ffprobe -loglevel warning -show_streams -select_streams a test.ts | grep sample_rate=48000

		# output timestamp check as a script to reuse for post-transcoding check
		cat <<- 'EOF' > check_ts
			set -eux
			# ensure 1 second of timestamps add up to within 2.1% of 90khz (mpegts timebase)
			# 2.1% is the margin of error, 1024 / 48000 (% increase per frame)
			# 1024 = samples per frame, 48000 = samples per second

			# select last frame pts, subtract from first frame pts, check diff
			ffprobe -loglevel warning -show_frames  -select_streams a "$2"  | grep pts= | head -"$1" | awk 'BEGIN{FS="="} ; NR==1 { fst = $2 } ; END{ diff=(($2-fst)/90000); exit diff <= 0.979 || diff >= 1.021 }'
		EOF
		chmod +x check_ts

		# check timestamps at the given frame offsets. 47 = ceil(48000/1024)
		./check_ts 47 test.ts

		# check failing cases; use +2 since we may be +/- the margin of error
		[ $(./check_ts 45 test.ts || echo "shouldfail") = "shouldfail" ]
		[ $(./check_ts 49 test.ts || echo "shouldfail") = "shouldfail" ]
	`
	run(cmd)

	err := Transcode(dir+"/test.ts", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	// Ensure transcoded sample rate is 44k.1hz and check timestamps
	cmd = `
		ffprobe -loglevel warning -show_streams -select_streams a out0test.ts | grep sample_rate=44100

		# Sample rate = 44.1khz, samples per frame = 1024
		# Frames per second = ceil(44100/1024) = 44

		# Technically check_ts margin of error is 2.1% due to 48khz rate
		# At 44.1khz, error is 2.3% so we'll just accept the tighter bounds

		# check timestamps at the given frame offsets. 44 = ceil(48000/1024)
		./check_ts 44 out0test.ts

		# check failing cases; use +2 since we may be +/- the margin of error
		[ $(./check_ts 46 out0test.ts || echo "shouldfail") = "shouldfail" ]
		[ $(./check_ts 42 out0test.ts || echo "shouldfail") = "shouldfail" ]
	`
	run(cmd)

}

func TestTranscoder_Timestamp(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
		# prepare the input and sanity check 60fps
		cp "$1/../transcoder/test.ts" inp.ts
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames inp.ts > inp.out
		grep avg_frame_rate=60 inp.out
		grep r_frame_rate=60 inp.out

		# reduce 60fps original to 30fps indicated but 15fps real
		ffmpeg -loglevel warning -i inp.ts -an -vf 'fps=30,select=not(mod(n\,2))' -c:v libx264 -t 1 -fps_mode vfr test.ts
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames test.ts > test.out

		# sanity check some properties. hard code numbers for now.
		grep avg_frame_rate=30 test.out
		grep r_frame_rate=15 test.out
		grep nb_read_frames=15 test.out
		grep duration_ts=90000 test.out
		grep start_pts=138000 test.out
	`
	run(cmd)

	err := Transcode(dir+"/test.ts", dir, []VideoProfile{P240p30fps16x9})
	if err != nil {
		t.Error(err)
	}

	cmd = `
		# hardcode some checks for now. TODO make relative to source.
		ffprobe -loglevel warning -select_streams v -show_streams -count_frames out0test.ts > test.out

		grep avg_frame_rate=30 test.out
		grep r_frame_rate=30 test.out
		grep nb_read_frames=29 test.out
		grep duration_ts=87000 test.out
		grep start_pts=138000 test.out
	`
	run(cmd)
}

func TestTranscoderStatistics_Decoded(t *testing.T) {
	// Checks the decoded stats returned after transcoding

	var (
		totalPixels int64
		totalFrames int
	)

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// segment using our muxer. This should produce 4 segments.
	err := RTMPToHLS("../transcoder/test.ts", dir+"/test.m3u8", dir+"/test_%d.ts", "1", 0)
	if err != nil {
		t.Error(err)
	}

	// Use various resolutions to test input
	// Quickcheck style tests would be nice here one day?
	profiles := []VideoProfile{P144p30fps16x9, P240p30fps16x9, P360p30fps16x9, P576p30fps16x9}

	// Transcode some data, save encoded statistics, then attempt to re-transcode
	// Ensure decoded re-transcode stats match original transcoded statistics
	for i, p := range profiles {
		oname := fmt.Sprintf("%s/out_%d.ts", dir, i)
		out := []TranscodeOptions{{Profile: p, Oname: oname}}
		in := &TranscodeOptionsIn{Fname: fmt.Sprintf("%s/test_%d.ts", dir, i)}
		res, err := Transcode3(in, out)
		if err != nil {
			t.Error(err)
		}
		info := res.Encoded[0]

		// Now attempt to re-encode the transcoded data
		// Pass in an empty output to achieve a decode-only flow
		// and check decoded results from *that*
		in = &TranscodeOptionsIn{Fname: oname}
		res, err = Transcode3(in, nil)
		if err != nil {
			t.Error(err)
		}
		w, h, err := VideoProfileResolution(p)
		if err != nil {
			t.Error(err)
		}

		// Check pixel counts
		if info.Pixels != res.Decoded.Pixels {
			t.Error("Mismatched pixel counts")
		}
		if info.Pixels != int64(w*h*res.Decoded.Frames) {
			t.Error("Mismatched pixel counts")
		}
		// Check frame counts
		if info.Frames != res.Decoded.Frames {
			t.Error("Mismatched frame counts")
		}
		if info.Frames != int(res.Decoded.Pixels/int64(w*h)) {
			t.Error("Mismatched frame counts")
		}
		totalPixels += info.Pixels
		totalFrames += info.Frames
	}

	// Now for something fun. Concatenate our segments of various resolutions
	// Run them through the transcoder, and check the sum of pixels / frames match
	// Ensures we can properly accommodate mid-stream resolution changes.
	cmd := `
        cat out_0.ts out_1.ts out_2.ts out_3.ts > combined.ts
    `
	run(cmd)
	in := &TranscodeOptionsIn{Fname: dir + "/combined.ts"}
	res, err := Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	if totalPixels != res.Decoded.Pixels {
		t.Error("Mismatched total pixel counts")
	}
	if totalFrames != res.Decoded.Frames {
		t.Errorf("Mismatched total frame counts - %d vs %d", totalFrames, res.Decoded.Frames)
	}
}

func TestTranscoder_Statistics_Encoded(t *testing.T) {
	// Checks the encoded stats returned after transcoding

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        # prepare 1-second input
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -loglevel warning -i inp.ts -c:a copy -c:v copy -t 1 test.ts
    `
	run(cmd)

	// set a 60fps input at a small resolution (to help runtime)
	p144p60fps := P144p30fps16x9
	p144p60fps.Framerate = 60
	// odd / nonstandard input just to sanity check.
	podd123fps := VideoProfile{Resolution: "146x82", Framerate: 123, Bitrate: "100k"}

	// Construct output parameters.
	// Quickcheck style tests would be nice here one day?
	profiles := []VideoProfile{P240p30fps16x9, P144p30fps16x9, p144p60fps, podd123fps}
	out := make([]TranscodeOptions, len(profiles))
	for i, p := range profiles {
		out[i] = TranscodeOptions{Profile: p, Oname: fmt.Sprintf("%s/out%d.ts", dir, i)}
	}

	res, err := Transcode3(&TranscodeOptionsIn{Fname: dir + "/test.ts"}, out)
	if err != nil {
		t.Error(err)
	}

	for i, r := range res.Encoded {
		w, h, err := VideoProfileResolution(out[i].Profile)
		if err != nil {
			t.Error(err)
		}

		// Check pixel counts
		if r.Pixels != int64(w*h*r.Frames) {
			t.Error("Mismatched pixel counts")
		}
		// Since this is a 1-second input we should ideally have count of frames
		if r.Frames != int(out[i].Profile.Framerate+1) {

			// Some "special" cases (already have test cases covering these)
			if p144p60fps == out[i].Profile {
				if r.Frames != int(out[i].Profile.Framerate)+1 {
					t.Error("Mismatched frame counts for 60fps; expected 61 frames but got ", r.Frames)
				}
			} else if podd123fps == out[i].Profile {
				if r.Frames != 124 {
					t.Error("Mismatched frame counts for 123fps; expected 124 frames but got ", r.Frames)
				}
			} else {
				t.Error("Mismatched frame counts ", r.Frames, out[i].Profile.Framerate)
			}
		}

		// Check frame counts against ffprobe-reported output

		// First, generate stats file
		f, err := os.Create(fmt.Sprintf("%s/out%d.res.stats", dir, i))
		if err != nil {
			t.Error(err)
		}
		b := bufio.NewWriter(f)
		fmt.Fprintf(b, `width=%d
height=%d
nb_read_frames=%d
`, w, h, r.Frames)
		b.Flush()
		f.Close()

		cmd = fmt.Sprintf(`
            fname=out%d

            ffprobe -loglevel warning -hide_banner -count_frames -count_packets  -select_streams v -show_streams 2>&1 $fname.ts | grep '^width=\|^height=\|nb_read_frames=' > $fname.stats

            diff -u $fname.stats $fname.res.stats
		`, i)

		run(cmd)
	}
}

func TestTranscoder_StatisticsAspectRatio(t *testing.T) {
	// Check that we correctly account for aspect ratio adjustments
	//  Eg, the transcoded resolution we receive may be smaller than
	//  what we initially requested

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        # prepare 1-second input
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -loglevel warning -i inp.ts -c:a copy -c:v copy -t 1 test.ts
    `
	run(cmd)

	// This will be adjusted to 146x82 by the rescaler (since source is 16:9)
	pAdj := VideoProfile{Resolution: "0x123", Framerate: 16, Bitrate: "100k"}
	out := []TranscodeOptions{{Profile: pAdj, Oname: dir + "/adj.mp4"}}
	res, err := Transcode3(&TranscodeOptionsIn{Fname: dir + "/test.ts"}, out)
	if err != nil || len(res.Encoded) <= 0 {
		t.Error(err)
	}
	r := res.Encoded[0]
	if r.Frames != int(pAdj.Framerate+1) || r.Pixels != int64(r.Frames*146*82) {
		t.Error(fmt.Errorf("Results did not match: %v ", r))
	}
}

func TestTranscoder_MuxerOpts(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Prepare test environment : truncate input file
	cmd := `
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -i inp.ts -c:a copy -c:v copy -t 1 inp-short.ts
    `
	run(cmd)

	prof := P240p30fps16x9

	// Set the muxer itself given a different extension
	_, err := Transcode3(&TranscodeOptionsIn{
		Fname: dir + "/inp-short.ts",
	}, []TranscodeOptions{{
		Oname:   dir + "/out-mkv.mp4",
		Profile: prof,
		Muxer:   ComponentOptions{Name: "matroska"},
	}})

	if err != nil {
		t.Error(err)
	}

	// Pass in some options to muxer
	_, err = Transcode3(&TranscodeOptionsIn{
		Fname: dir + "/inp.ts",
	}, []TranscodeOptions{{
		Oname:   dir + "/out.mpd",
		Profile: prof,
		Muxer: ComponentOptions{
			Name: "dash",
			Opts: map[string]string{
				"media_seg_name": "lpms-test-$RepresentationID$-$Number%05d$.m4s",
				"init_seg_name":  "lpms-init-$RepresentationID$.m4s",
			},
		},
	}})
	if err != nil {
		t.Error(err)
	}

	cmd = `
        # check formats and that options were used
        ffprobe -loglevel warning -show_format out-mkv.mp4 | grep format_name=matroska
        # ffprobe -loglevel warning -show_format out.mpd | grep format_name=dash # this fails so skip for now

        # concat headers. mp4 chunks are annoying
        cat lpms-init-0.m4s lpms-test-0-00001.m4s > video.m4s
        cat lpms-init-1.m4s lpms-test-1-00001.m4s > audio.m4s
        ffprobe -show_format video.m4s | grep nb_streams=1
        ffprobe -show_format audio.m4s | grep nb_streams=1
        ffprobe -show_streams -select_streams v video.m4s | grep codec_name=h264
        ffprobe -show_streams -select_streams a audio.m4s | grep codec_name=aac
    `
	run(cmd)
}

type TranscodeOptionsTest struct {
	InputCodec  VideoCodec
	OutputCodec VideoCodec
	InputAccel  Acceleration
	OutputAccel Acceleration
	Profile     VideoProfile
}

func TestSW_Transcoding(t *testing.T) {
	codecsComboTest(t, supportedCodecsCombinations([]Acceleration{Software}))
}

func supportedCodecsCombinations(accels []Acceleration) []TranscodeOptionsTest {
	prof := P240p30fps16x9
	var opts []TranscodeOptionsTest
	inCodecs := []VideoCodec{H264, H265, VP8, VP9}
	outCodecs := []VideoCodec{H264, H265, VP8, VP9}
	for _, inAccel := range accels {
		for _, outAccel := range accels {
			for _, inCodec := range inCodecs {
				for _, outCodec := range outCodecs {
					// skip unsupported combinations
					switch outAccel {
					case Nvidia:
						switch outCodec {
						case VP8, VP9:
							continue
						}
					}
					opts = append(opts, TranscodeOptionsTest{
						InputCodec:  inCodec,
						OutputCodec: outCodec,
						InputAccel:  inAccel,
						OutputAccel: outAccel,
						Profile:     prof,
					})
				}
			}
		}
	}
	return opts
}

func codecsComboTest(t *testing.T, options []TranscodeOptionsTest) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	sampleName := dir + "/test.ts"
	var inName, outName, qName string
	cmd := `
    # set up initial input; truncate test.ts file
    ffmpeg -loglevel warning -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test.ts
  `
	run(cmd)
	var err error
	for i := range options {
		curOptions := options[i]
		switch curOptions.InputCodec {
		case VP8, VP9:
			inName = dir + "/test_in.mkv"
		case H264, H265:
			inName = dir + "/test_in.ts"
		}
		switch curOptions.OutputCodec {
		case VP8, VP9:
			outName = dir + "/out.mkv"
			qName = dir + "/sw.mkv"
		case H264, H265:
			outName = dir + "/out.ts"
			qName = dir + "/sw.ts"
		}
		// if non-h264 test requested, transcode to target input codec first
		prepare := true
		if curOptions.InputCodec != H264 {
			profile := P720p60fps16x9
			profile.Encoder = curOptions.InputCodec
			err = Transcode2(&TranscodeOptionsIn{
				Fname: sampleName,
				Accel: Software,
			}, []TranscodeOptions{
				{
					Oname:   inName,
					Profile: profile,
					Accel:   Software,
				},
			})
			if err != nil {
				t.Error(err)
				prepare = false
			}
		} else {
			inName = sampleName
		}
		targetProfile := curOptions.Profile
		targetProfile.Encoder = curOptions.OutputCodec
		transcode := prepare
		if prepare {
			err = Transcode2(&TranscodeOptionsIn{
				Fname: inName,
				Accel: curOptions.InputAccel,
			}, []TranscodeOptions{
				{
					Oname:   outName,
					Profile: targetProfile,
					Accel:   curOptions.OutputAccel,
				},
			})
			if err != nil {
				t.Error(err)
				transcode = false
			}
		}
		quality := transcode
		if transcode {
			// software transcode for image quality check
			err = Transcode2(&TranscodeOptionsIn{
				Fname: inName,
				Accel: Software,
			}, []TranscodeOptions{
				{
					Oname:   qName,
					Profile: targetProfile,
					Accel:   Software,
				},
			})
			if err != nil {
				t.Error(err)
				quality = false
			}
			cmd = fmt.Sprintf(`
    # compare using ssim and generate stats file
    ffmpeg -loglevel warning -i %s -i %s -lavfi '[0:v][1:v]ssim=stats.log' -f null -
    # check image quality; ensure that no more than 5 frames have ssim < 0.95
    grep -Po 'All:\K\d+.\d+' stats.log | awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'
  `, outName, qName)
			if quality {
				quality = run(cmd)
			}
		}
		t.Logf("Transcode %s (Accel: %d) -> %s (Accel: %d) Prepare: %t Transcode: %t Quality: %t\n",
			VideoCodecName[curOptions.InputCodec],
			curOptions.InputAccel,
			VideoCodecName[curOptions.OutputCodec],
			curOptions.OutputAccel,
			prepare, transcode, quality)
	}
}

func TestTranscoder_EncoderOpts(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Prepare test environment : truncate input file
	cmd := `
        # truncate input
        ffmpeg -i "$1/../transcoder/test.ts" -c:a copy -c:v copy -t 1 test.ts

        # we will sanity check image quality with ssim
        # since ssim needs res and framecount to match, sanity check those
        ffprobe -show_streams -select_streams v test.ts | grep width=1280
        ffprobe -show_streams -select_streams v test.ts | grep height=720
        ffprobe -count_frames -show_streams -select_streams v test.ts | grep nb_read_frames=60
    `
	run(cmd)

	prof := P720p60fps16x9
	in := &TranscodeOptionsIn{Fname: dir + "/test.ts"}
	out := []TranscodeOptions{{
		Oname:        dir + "/out.nut",
		Profile:      prof,
		VideoEncoder: ComponentOptions{Name: "snow"},
		AudioEncoder: ComponentOptions{
			Name: "vorbis",
			// required since vorbis implementation is marked experimental
			// also, gives us an opportunity to test the audio opts
			Opts: map[string]string{"strict": "experimental"}},
	}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}

	cmd = `
        # Check codecs are what we expect them to be
        ffprobe -show_streams -select_streams v out.nut | grep codec_name=snow
        ffprobe -show_streams -select_streams a out.nut | grep codec_name=vorbis

        # sanity check image quality : compare using ssim
        ffmpeg -loglevel warning -i out.nut -i test.ts -lavfi '[0:v][1:v]ssim=stats.log' -f null -
        # ensure that no more than 5 frames have ssim < 0.95
        grep -Po 'All:\K\d+.\d+' stats.log | awk '{ if ($1 < 0.95) count=count+1 } END{ exit count > 5 }'
    `
	run(cmd)
}

func TestTranscoder_StreamCopy(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Set up inputs, truncate test file
	cmd := `
        cp "$1"/../transcoder/test.ts .
        ffmpeg -i test.ts -c:a copy -c:v copy -t 1 test-short.ts

        # sanity check some assumptions here for the following set of tests
        ffprobe -count_frames -show_streams -select_streams v test-short.ts | grep nb_read_frames=60
    `
	run(cmd)

	// Test normal stream-copy case
	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	out := []TranscodeOptions{
		{
			Oname:        dir + "/audiocopy.ts",
			Profile:      P144p30fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
		},
		{
			Oname: dir + "/videocopy.ts",
			VideoEncoder: ComponentOptions{Name: "copy", Opts: map[string]string{
				"mpegts_flags": "resend_headers,initial_discontinuity",
			}},
		},
	}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 60 || res.Encoded[0].Frames != 31 ||
		res.Encoded[1].Frames != 0 {
		t.Error("Unexpected frame counts from stream copy")
		t.Error(res)
	}

	cmd = `
        # extract video track only, compare md5sums
        ffmpeg -i test-short.ts -an -c:v copy -f md5 test-video.md5
        ffmpeg -i videocopy.ts -an -c:v copy -f md5 videocopy.md5
        diff -u test-video.md5 videocopy.md5

        # extract audio track only, compare md5sums
        ffmpeg -i test-short.ts -vn -c:a copy -f md5 test-audio.md5
        ffmpeg -i audiocopy.ts -vn -c:a copy -f md5 audiocopy.md5
        diff -u test-audio.md5 audiocopy.md5
    `
	run(cmd)

	// Test stream copy when no stream exists in file
	cmd = `
        ffmpeg -i test-short.ts -an -c:v copy videoonly.ts
        ffmpeg -i test-short.ts -vn -c:a copy audioonly.ts

    `
	run(cmd)
	in = &TranscodeOptionsIn{Fname: dir + "/videoonly.ts"}
	out = []TranscodeOptions{
		{
			Oname:        dir + "/novideo.ts",
			VideoEncoder: ComponentOptions{Name: "copy"},
		},
	}
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 0 || res.Encoded[0].Frames != 0 {
		t.Error("Unexpected count of decoded/encoded frames")
	}
	in = &TranscodeOptionsIn{Fname: dir + "/audioonly.ts"}
	out = []TranscodeOptions{
		{
			Oname:        dir + "/noaudio.ts",
			Profile:      P144p30fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
		},
	}
	// Audio only segments are not supported
	_, err = Transcode3(in, out)
	assert.EqualError(t, err, "TranscoderInvalidVideo")
}

func TestTranscoder_StreamCopy_Validate_B_Frames(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Set up inputs, truncate test file
	cmd := `

        ffmpeg -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test-short.ts

        # sanity check some assumptions here for the following set of tests
        ffprobe -count_frames -show_streams -select_streams v test-short.ts | grep nb_read_frames=60

		# Sanity check that we have B-frames in this sample
        ffprobe -show_frames test-short.ts | grep pict_type=B
    `
	run(cmd)

	// Test normal stream-copy case
	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	out := []TranscodeOptions{
		{
			Oname:        dir + "/videocopy.ts",
			VideoEncoder: ComponentOptions{Name: "copy"},
		},
	}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 0 || res.Encoded[0].Frames != 0 {
		t.Error("Unexpected frame counts from stream copy")
		t.Error(res)
	}

	cmd = `

        # extract video track only, compare md5sums
        ffmpeg -i test-short.ts -an -c:v copy -f md5 test-video.md5
        ffmpeg -i videocopy.ts -an -c:v copy -f md5 videocopy.md5
        diff -u test-video.md5 videocopy.md5

		# ensure output has equal no of B-Frames as input		
		ffprobe -loglevel warning -show_frames -select_streams v -show_entries frame=pict_type videocopy.ts | grep pict_type=B | wc -l > read_pict_type.out
		ffprobe -loglevel warning -show_frames -select_streams v -show_entries frame=pict_type test-short.ts | grep pict_type=B | wc -l > ffmpeg_read_pict_type.out
		diff -u ffmpeg_read_pict_type.out read_pict_type.out
    `
	run(cmd)

}

func TestTranscoder_Drop(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        cp "$1"/../transcoder/test.ts .
        ffmpeg -i test.ts -c:a copy -c:v copy -t 1 test-short.ts

        # sanity check some assumptions here for the following set of tests
        ffprobe -count_frames -show_streams -select_streams v test-short.ts | grep nb_read_frames=60
    `
	run(cmd)

	// Normal case : drop only video
	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	out := []TranscodeOptions{
		{
			Oname:        dir + "/novideo.ts",
			VideoEncoder: ComponentOptions{Name: "drop"},
		},
	}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 0 || res.Encoded[0].Frames != 0 {
		t.Error("Unexpected count of decoded frames ", res.Decoded.Frames, res.Decoded.Pixels)
	}

	// Normal case: drop only audio
	out = []TranscodeOptions{
		{
			Oname:        dir + "/noaudio.ts",
			AudioEncoder: ComponentOptions{Name: "drop"},
			Profile:      P144p30fps16x9,
		},
	}
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 60 || res.Encoded[0].Frames != 31 {
		t.Error("Unexpected count of decoded frames ", res.Decoded.Frames, res.Decoded.Pixels)
	}

	// Test error when trying to mux no streams
	out = []TranscodeOptions{{
		Oname:        dir + "/none.mp4",
		VideoEncoder: ComponentOptions{Name: "drop"},
		AudioEncoder: ComponentOptions{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err == nil || err.Error() != "Invalid argument" {
		t.Error("Did not get expected error: ", err)
	}

	// Test error when missing profile in default video configuration
	out = []TranscodeOptions{{
		Oname:        dir + "/profile.mp4",
		AudioEncoder: ComponentOptions{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err == nil || err != ErrTranscoderRes {
		t.Error("Expected res err related to profile, but got ", err)
	}

	// Sanity check default transcode options with single-stream input
	in.Fname = dir + "/noaudio.ts"
	out = []TranscodeOptions{{Oname: dir + "/encoded-video.mp4", Profile: P144p30fps16x9}}
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Decoded.Frames != 31 || res.Encoded[0].Frames != 31 {
		t.Error("Unexpected encoded/decoded frame counts ", res.Decoded.Frames, res.Encoded[0].Frames)
	}
	in.Fname = dir + "/novideo.ts"
	out = []TranscodeOptions{{Oname: dir + "/encoded-audio.mp4", Profile: P144p30fps16x9}}
	_, err = Transcode3(in, out)
	// Audio only segments are not supported
	assert.EqualError(t, err, "TranscoderInvalidVideo")
}

func TestTranscoder_StreamCopyAndDrop(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	out := []TranscodeOptions{{
		Oname:        dir + "/videoonly.mp4",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "drop"},
	}, {
		Oname:        dir + "/audioonly.mp4",
		VideoEncoder: ComponentOptions{Name: "drop"},
		AudioEncoder: ComponentOptions{Name: "copy"},
	}, {
		// Avoids ADTS to ASC conversion
		// which changes the bitstream
		Oname:        dir + "/audioonly.ts",
		VideoEncoder: ComponentOptions{Name: "drop"},
		AudioEncoder: ComponentOptions{Name: "copy"},
	}, {
		Oname:        dir + "/audio.md5",
		VideoEncoder: ComponentOptions{Name: "drop"},
		AudioEncoder: ComponentOptions{Name: "copy"},
		Muxer:        ComponentOptions{Name: "md5"},
	}, {
		Oname:        dir + "/video.md5",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "drop"},
		Muxer:        ComponentOptions{Name: "md5"},
	}, {
		Oname:        dir + "/copy.mp4",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "copy"},
	}}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res != nil {
		if res.Decoded.Frames != 0 {
			t.Error("Unexpected count for decoded frames ", res.Decoded.Frames)
		}
	}
	cmd := `
        cp "$1"/../transcoder/test.ts .

        # truncate input for later use
        ffmpeg -i test.ts -t 1 -c:a copy -c:v copy test-short.ts

        # check some results
        ffprobe -loglevel warning -show_format videoonly.mp4 | grep nb_streams=1
        ffprobe -loglevel warning -show_streams videoonly.mp4 | grep codec_name=h264

        ffprobe -loglevel warning -show_format audioonly.mp4 | grep nb_streams=1
        ffprobe -loglevel warning -show_streams audioonly.mp4 | grep codec_name=aac

        ffprobe -loglevel warning -show_format copy.mp4 | grep nb_streams=2

        # Verify video md5sum
        ffmpeg -i test.ts -an -c:v copy -f md5 ffmpeg-video-orig.md5
        diff -u video.md5 ffmpeg-video-orig.md5

        # Verify audio md5sums
        ffmpeg -i test.ts -vn -c:a copy -f md5 ffmpeg-audio-orig.md5
        ffmpeg -i audioonly.ts -vn -c:a copy -f md5 ffmpeg-audio-ts.md5
        ffmpeg -i copy.mp4 -vn -c:a copy -f md5 ffmpeg-audio-copy.md5
        ffmpeg -i audioonly.mp4 -c:a copy -f md5 ffmpeg-audio-mp4.md5
        diff -u audio.md5 ffmpeg-audio-orig.md5
        diff -u audio.md5 ffmpeg-audio-ts.md5
        diff -u ffmpeg-audio-mp4.md5 ffmpeg-audio-copy.md5

        # TODO test timestamps? should they be copied?
    `
	run(cmd)

	// Test specifying a copy or a drop for a stream that does not exist
	in.Fname = dir + "/videoonly.mp4"
	out = []TranscodeOptions{{
		Oname:        dir + "/videoonly-copy.mp4",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "copy"},
	}, {
		Oname:        dir + "/videoonly-copy-2.mp4",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err, in.Fname)
	}

	// Test mp4-to-mpegts; involves an implicit bitstream conversion to annex B
	in.Fname = dir + "/videoonly.mp4"
	out = []TranscodeOptions{{
		Oname:        dir + "/videoonly-copy.ts",
		VideoEncoder: ComponentOptions{Name: "copy"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	// sanity check the md5sum of the mp4-to-mpegts result
	in.Fname = dir + "/videoonly-copy.ts"
	out = []TranscodeOptions{{
		Oname:        dir + "/videoonly-copy.md5",
		VideoEncoder: ComponentOptions{Name: "copy"},
		Muxer:        ComponentOptions{Name: "md5"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        # use ffmpeg to convert the existing mp4 to ts and check match
        # for some reason this does NOT match the original mpegts
        ffmpeg -i videoonly.mp4 -c:v copy -f mpegts ffmpeg-mp4to.ts
        ffmpeg -i ffmpeg-mp4to.ts -c:v copy -f md5 ffmpeg-mp4tots.md5
        diff -u videoonly-copy.md5 ffmpeg-mp4tots.md5
    `
	run(cmd)

	// Encode one stream of a short sample while copying / dropping another
	in.Fname = dir + "/test-short.ts"
	out = []TranscodeOptions{{
		Oname:        dir + "/encoded-video.mp4",
		Profile:      P144p30fps16x9,
		AudioEncoder: ComponentOptions{Name: "drop"},
	}, {
		Oname:        dir + "/encoded-audio.mp4",
		VideoEncoder: ComponentOptions{Name: "drop"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)

	}
}

func TestTranscoder_RepeatedTranscodes(t *testing.T) {

	// We have an issue where for certain inputs, we get a few more frames
	// if trying to transcode to the same framerate.
	// This test is to ensure that those errors don't compound.

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        ffmpeg -i "$1"/../transcoder/test.ts -an -c:v copy -t 1 test-short.ts
        ffprobe -count_frames -show_streams test-short.ts > test.stats
        grep nb_read_frames=62 test.stats

        # this is informative: including audio makes things a bit more "expected"
        ffmpeg -i "$1"/../transcoder/test.ts -c:a copy -c:v copy -t 1 test-short-with-audio.ts
        ffprobe -count_frames -show_streams -select_streams v test-short-with-audio.ts > test-with-audio.stats
        grep nb_read_frames=60 test-with-audio.stats
    `
	run(cmd)

	// Initial set up; convert 60fps input to 30fps (1 second's worth)
	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	out := []TranscodeOptions{{Oname: dir + "/0.ts", Profile: P144p30fps16x9}}
	res, err := Transcode3(in, out)
	if err != nil || res.Decoded.Frames != 62 || res.Encoded[0].Frames != 31 {
		t.Error("Unexpected preconditions ", err, res)
	}
	frames := res.Encoded[0].Frames

	// Transcode results repeatedly, ensuring we have the same results each time
	for i := 0; i < 5; i++ {
		in.Fname = fmt.Sprintf("%s/%d.ts", dir, i)
		out[0].Oname = fmt.Sprintf("%s/%d.ts", dir, i+1)
		res, err = Transcode3(in, out)
		if err != nil ||
			res.Decoded.Frames != frames || res.Encoded[0].Frames != frames {
			t.Error(fmt.Sprintf("Unexpected frame count for input %d : decoded %d encoded %d", i, res.Decoded.Frames, res.Encoded[0].Frames))
		}
		if res.Decoded.Frames != 31 && res.Encoded[0].Frames != 31 {
			t.Error("Unexpected frame count! ", res)
		}
	}

	// Do the same but with audio. This yields a 30fps file.
	in = &TranscodeOptionsIn{Fname: dir + "/test-short-with-audio.ts"}
	out = []TranscodeOptions{{Oname: dir + "/audio-0.ts", Profile: P144p30fps16x9}}
	res, err = Transcode3(in, out)
	if err != nil || res.Decoded.Frames != 60 || res.Encoded[0].Frames != 31 {
		t.Error("Unexpected preconditions ", err, res)
	}
	frames = res.Encoded[0].Frames

	// Transcode results repeatedly, ensuring we have the same results each time
	for i := 0; i < 5; i++ {
		in.Fname = fmt.Sprintf("%s/audio-%d.ts", dir, i)
		out[0].Oname = fmt.Sprintf("%s/audio-%d.ts", dir, i+1)
		res, err = Transcode3(in, out)
		if err != nil ||
			res.Decoded.Frames != frames || res.Encoded[0].Frames != frames {
			t.Error(fmt.Sprintf("Unexpected frame count for input %d : decoded %d encoded %d", i, res.Decoded.Frames, res.Encoded[0].Frames))
		}
	}
}

func TestTranscoder_MismatchedEncodeDecode(t *testing.T) {
	// ALL FIXED, Keeping it around as a sanity check for now
	// Encoded frame count does not match decoded frame count for mp4
	// Note this is not an issue for mpegts! (this is sanity checked)
	// See: https://github.com/livepeer/lpms/issues/155

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	p144p60fps := P144p30fps16x9
	p144p60fps.Framerate = 60

	cmd := `
        # prepare 1-second input
        cp "$1/../transcoder/test.ts" inp.ts
        ffmpeg -loglevel warning -i inp.ts -c:a copy -c:v copy -t 1 test.ts
        ffprobe -count_frames -show_streams -select_streams v test.ts | grep nb_read_frames=60
    `
	run(cmd)

	in := &TranscodeOptionsIn{Fname: dir + "/test.ts"}
	out := []TranscodeOptions{{Oname: dir + "/out.mp4", Profile: p144p60fps}}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	in.Fname = dir + "/out.mp4"
	res2, err := Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	// TODO Ideally these two should match. As far as I can tell it is due
	//      to timestamp rounding around EOF. Note this does not happen with
	//      mpegts formatted output!
	if res2.Decoded.Frames != 61 || res.Encoded[0].Frames != 61 {
		t.Error("Did not get expected frame counts: check if issue #155 is fixed!",
			res2.Decoded.Frames, res.Encoded[0].Frames)
	}
	cmd = `
        ffprobe -count_frames -show_packets -show_streams -select_streams v out.mp4 2>&1 > mp4.out
        grep nb_read_frames=61 mp4.out
        grep nb_read_packets=61 mp4.out

        # also ensure that we match ffmpeg's own frame count for the same type of encode
        ffmpeg -i test.ts -vf 'fps=60/1,scale=144x60' -c:v libx264 -c:a copy ffmpeg.mp4
        ffprobe -count_frames -show_packets -show_streams -select_streams v out.mp4 2>&1 > ffmpeg-mp4.out
        grep nb_read_frames=61 ffmpeg-mp4.out
        grep nb_read_packets=61 ffmpeg-mp4.out
    `
	run(cmd)

	// Sanity check mpegts works as expected
	in.Fname = dir + "/test.ts"
	out[0].Oname = dir + "/out.ts"
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	in.Fname = dir + "/out.ts"
	res2, err = Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	if res2.Decoded.Frames != 61 || res.Encoded[0].Frames != 61 {
		t.Error("Did not get expected frame counts for mpegts ",
			res2.Decoded.Frames, res.Encoded[0].Frames)
	}

	// Sanity check we still get the same results with multiple outputs?
	in.Fname = dir + "/test.ts"
	out = []TranscodeOptions{
		{Oname: dir + "/out2.ts", Profile: p144p60fps},
		{Oname: dir + "/out2.mp4", Profile: p144p60fps},
	}
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	in.Fname = dir + "/out2.ts"
	res2, err = Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	in.Fname = dir + "/out2.mp4"
	res3, err := Transcode3(in, nil)
	if err != nil {
		t.Error(err)
	}
	// first output is mpegts
	if res2.Decoded.Frames != 61 || res.Encoded[0].Frames != 61 {
		t.Error("Sanity check of mpegts failed ", res2.Decoded.Frames, res.Encoded[0].Frames)
	}
	if res3.Decoded.Frames != 61 || res.Encoded[1].Frames != 61 {
		t.Error("Sanity check of mp4 failed ", res3.Decoded.Frames, res.Encoded[1].Frames)
	}
}

func TestTranscoder_FFmpegMatching(t *testing.T) {
	// Sanity check that ffmpeg matches the following behavior:

	// No audio case
	// 1 second input, N fps ( M frames; N != M for $reasons )
	// Output set to N/2 fps . Output contains ( M / 2 ) frames

	// TODO Unable to compare the frame counts in the following since diverged FPS handling from FFmpeg
	// Even though the LPMS frame counts make sense on their own

	// Other cases with audio result in frame count matching FPS
	// 1 second input, N fps ( N frames )
	// Output set to M fps. Output contains M frames

	// Weird framerate case
	// 1 second input, N fps ( N frames )
	// Output set to 123 fps. Output contains 125 frames

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
        cp "$1/../transcoder/test.ts" test.ts

        # Test no audio case
        ffmpeg -loglevel warning -i test.ts -an -c:v copy -t 1 test-noaudio.ts
        # sanity check one stream, and that no audio still has 60fps
		ffprobe -count_frames -show_streams -show_format test-noaudio.ts 2>&1 > noaudio.stats
        grep avg_frame_rate=60 noaudio.stats
        grep nb_read_frames=62 noaudio.stats
        grep nb_streams=1 noaudio.stats
        grep codec_name=h264 noaudio.stats


        ffmpeg -loglevel warning -i test.ts -c:a copy -c:v copy -t 1 test-60fps.ts
		ffprobe -show_streams test-60fps.ts | grep avg_frame_rate=60

        ls -lha
    `
	run(cmd)

	checkStatsFile := func(in *TranscodeOptionsIn, out *TranscodeOptions, res *TranscodeResults) {
		// Generate stats file for given LPMS output
		f, err := os.Create(dir + "/lpms.stats")
		if err != nil {
			t.Error(err)
		}
		defer f.Close()
		w, h, err := VideoProfileResolution(out.Profile)
		if err != nil {
			t.Error("Invalid profile ", err)
		}
		b := bufio.NewWriter(f)
		fmt.Fprintf(b, `width=%d
height=%d
nb_read_frames=%d
`,
			w, h, res.Encoded[0].Frames)
		b.Flush()

		// Run a ffmpeg command that attempts to match the given encode settings
		run(fmt.Sprintf(`ffmpeg -loglevel warning -hide_banner -i %s -vsync passthrough -c:a aac -ar 44100 -ac 2 -c:v libx264 -vf fps=%d/1:eof_action=pass,scale=%dx%d -copyts -muxdelay 0 -y ffmpeg.ts`, in.Fname, out.Profile.Framerate, w, h))

		// Gather some ffprobe stats on the output of the above ffmpeg command
		run(`ffprobe -loglevel warning -hide_banner -count_frames -select_streams v -show_streams 2>&1 ffmpeg.ts | grep '^width=\|^height=\|nb_read_frames=' > ffmpeg.stats`)
		// Gather ffprobe stats on the output of the LPMS transcode itself
		run(fmt.Sprintf(`ffprobe -loglevel warning -hide_banner -count_frames -select_streams v -show_streams 2>&1 %s | grep '^width=\|^height=\|nb_read_frames=' > ffprobe-lpms.stats`, out.Oname))

		// Now ensure stats match across 3 files:
		// 1. ffmpeg encoded results, as checked by ffprobe (ffmpeg.stats)
		// 2. ffprobe-checked LPMS output (ffprobe-lpms.stats)
		// 3. Statistics received directly from LPMS after transcoding (lpms.stat)
		cmd := `
            diff -u ffmpeg.stats lpms.stats
            diff -u ffprobe-lpms.stats lpms.stats
        `
		run(cmd)
	}

	// no audio + 60fps input
	in := &TranscodeOptionsIn{Fname: dir + "/test-noaudio.ts"}
	out := []TranscodeOptions{{
		Oname:   dir + "/out-noaudio.ts",
		Profile: P144p30fps16x9,
	}}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Encoded[0].Frames != 31 {
		t.Error("Did not get expected frame count ", res.Encoded[0].Frames)
	}
	checkStatsFile(in, &out[0], res)

	// audio + 60fps input, 30fps output, 30 frames actual
	in.Fname = dir + "/test-60fps.ts"
	out[0].Oname = dir + "/out-60fps-to-30fps.ts"
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Encoded[0].Frames != 31 {
		t.Error("Did not get expected frame count ", res.Encoded[0].Frames)
	}
	checkStatsFile(in, &out[0], res)

	// audio + 60fps input, 60fps output. 60 frames actual
	in.Fname = dir + "/test-60fps.ts"
	out[0].Oname = dir + "/out-60-to-120fps.ts"
	out[0].Profile.Framerate = 60
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Encoded[0].Frames != 61 { //
		t.Error("Did not get expected frame count ", res.Encoded[0].Frames)
	}
	checkStatsFile(in, &out[0], res)

	// audio + 60fps input, 123 fps output. 123 frames actual
	in.Fname = dir + "/test-60fps.ts"
	out[0].Oname = dir + "/out-123fps.ts"
	out[0].Profile.Framerate = 123
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	if res.Encoded[0].Frames != 124 { // TODO Find out why this isn't 126 (ffmpeg)
		t.Error("Did not get expected frame count ", res.Encoded[0].Frames)
	}
	// checkStatsFile(in, &out[0], res) // TODO framecounts don't match ffmpeg
}

func TestTranscoder_PassthroughFPS(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Set  up test inputs and sanity check some things
	cmd := `
        cp "$1/../transcoder/test.ts" test.ts

        # Inputs
        ffmpeg -v warning -i test.ts -an -c:v copy -t 1 test-short.ts
        ffmpeg -v warning -i test-short.ts -c:v libx264 -vf fps=123,scale=256x144 test-123fps.mp4

        # Check stream properties (hard code expectations)
        ffprobe -v warning -show_streams -count_frames test-short.ts | grep nb_read_frames=62
        ffprobe -v warning -show_streams -count_frames test-123fps.mp4 | grep nb_read_frames=127
        ffprobe -v warning -show_streams test-short.ts | grep r_frame_rate=60/1
        ffprobe -v warning -show_streams test-123fps.mp4 | grep r_frame_rate=123/1
        # Extract frame properties for later comparison
        ffprobe -v warning -select_streams v -show_frames -show_entries frame=pts,pkt_dts,duration -of csv test-123fps.mp4 > test-123fps.data
        ffprobe -v warning -select_streams v -show_frames -show_entries frame=pts,pkt_dts,duration -of csv test-short.ts > test-short.data
    `
	run(cmd)
	out := []TranscodeOptions{{Profile: P144p30fps16x9}}
	out[0].Profile.Framerate = 0 // Passthrough!

	// Check a somewhat normal passthru case
	out[0].Oname = dir + "/out-short.ts"
	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	res, err := Transcode3(in, out)
	if err != nil {
		t.Error("Could not transcode: ", err)
	}
	if res.Encoded[0].Frames != 62 {
		t.Error("Did not get expected frame count; got ", res.Encoded[0].Frames)
	}

	// Now check odd frame rate
	out[0].Oname = dir + "/out-123fps.mp4"
	in.Fname = dir + "/test-123fps.mp4"
	res, err = Transcode3(in, out)
	if err != nil {
		t.Error("Could not transcode: ", err)
	}
	// yes, expecting 127 frames with 123fps set; this is odd after all!
	if res.Encoded[0].Frames != 127 {
		t.Error("Did not get expected frame count; got ", res.Encoded[0].Frames)
	}

	// Sanity check durations etc
	cmd = `
        # Check stream properties - should match original properties
        ffprobe -v warning -show_streams -count_frames out-short.ts | grep nb_read_frames=62
        ffprobe -v warning -show_streams -count_frames out-123fps.mp4 | grep nb_read_frames=127
        ffprobe -v warning -show_streams out-short.ts | grep r_frame_rate=60/1
        ffprobe -v warning -show_streams out-123fps.mp4 | grep r_frame_rate=123/1

        # Check some per-frame properties
        ffprobe -v warning -select_streams v -show_frames -show_entries frame=pts,pkt_dts,duration -of csv out-123fps.mp4 > out-123fps.data
        ffprobe -v warning -select_streams v -show_frames -show_entries frame=pts,pkt_dts,duration -of csv out-short.ts > out-short.data
        diff -u test-123fps.data out-123fps.data
        diff -u test-short.data test-short.data
    `
	run(cmd)
}

func TestTranscoder_PassthroughFPS_AdjustTimestamps(t *testing.T) {
	// check timestamp adjustments for fps passthrough

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
		ffmpeg -i "$1/../transcoder/test.ts" -an -c:v copy -t 0.5 test-short.ts
		ffprobe -loglevel warning -show_entries frame=pts,duration -of csv=p=0 test-short.ts | grep -v '^$' > expected-frame-pts.out
		wc -l expected-frame-pts.out | grep "32 expected-frame-pts.out"
		cat << EXPECTED_TS_EOF > expected-pkt-ts.out
pts,dts,duration
128970,125970,1500
134910,127410,1500
131940,128940,1500
130500,130500,1500
133380,131880,1500
137970,133470,1500
136440,134940,1500
143910,136410,1500
140940,137940,1500
139410,139410,1500
142470,140970,1500
149940,142440,1500
146970,143970,1500
145440,145440,1500
148500,147000,1500
155970,148470,1500
152910,149910,1500
151380,151380,1500
154440,152940,1500
161910,154410,1500
158940,155940,1500
157410,157410,1500
160470,158970,1500
167940,160440,1500
164970,161970,1500
163440,163440,1500
166500,165000,1500
173970,166470,1500
170910,167910,1500
169380,169380,1500
172440,170940,1500
176940,172440,1500
EXPECTED_TS_EOF
	`
	run(cmd)

	in := &TranscodeOptionsIn{Fname: dir + "/test-short.ts"}
	out := []TranscodeOptions{{Profile: P144p30fps16x9}}
	out[0].Profile.Framerate = 0 // Passthrough!
	out[0].Profile.Profile = ProfileH264High
	out[0].Oname = dir + "/out-0.ts"
	_, err := Transcode3(in, out)
	require.Nil(t, err)
	cmd = `
		echo "pts,dts,duration" > received-pkt-ts.out
		ffprobe -loglevel warning -show_entries packet=pts,dts,duration,pict_type -of csv=p=0 out-0.ts | grep -v '^$' | sed 's/,*$//g' >> received-pkt-ts.out
		ffprobe -loglevel warning -show_entries frame=pts,duration -of csv=p=0 test-short.ts | grep -v '^$' > received-frame-pts.out

		# ensure packet pts+dts matches what is expected
		diff -u expected-pkt-ts.out received-pkt-ts.out

		# ensure all pts are accounted for from original
		diff -u expected-frame-pts.out received-frame-pts.out
	`
	run(cmd)
}

func TestTranscoder_FormatOptions(t *testing.T) {
	// Test combinations of VideoProfile.Format and TranscodeOptions.Muxer
	// The former takes precedence over the latter if set

	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
        cp "$1/../transcoder/test.ts" test.ts
    `
	run(cmd)

	// First, sanity check VideoProfile defaults
	for _, p := range VideoProfileLookup {
		if p.Format != FormatNone {
			t.Error("Default VideoProfile format not set to FormatNone")
		}
	}

	// If no format and no mux opts specified, should be based on file extension
	in := &TranscodeOptionsIn{Fname: dir + "/test.ts"}
	out := []TranscodeOptions{{
		Oname:        dir + "/test.flv",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "copy"},
		Metadata: map[string]string{
			"encoded_by": "Livepeer Media Server",
		},
	}}
	if out[0].Profile.Format != FormatNone {
		t.Error("Expected empty profile for output option")
	}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        ffprobe -loglevel warning -show_format test.flv | grep 'format_name=flv\|encoded_by=Livepeer Media Server'
    `
	run(cmd)

	// If no format specified, should be based on given mux opts
	out[0].Oname = dir + "/actually_hls.flv" // sanity check extension ignored
	out[0].Muxer = ComponentOptions{Name: "hls", Opts: map[string]string{
		"hls_segment_filename": dir + "/test_segment_%d.ts",
	}}
	out[0].Metadata = map[string]string{
		"service_provider": "Livepeer Media Server",
	}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        # Check playlist
        head -1 actually_hls.flv | grep "#EXTM3U"
        # Check that (copied) mpegts stream matches source
        ls -lha test_segment_*.ts | wc -l | grep 4 # sanity check four segments
        cat test_segment_*.ts > segment.ts
        ffprobe -loglevel warning -show_entries format=format_name,duration segment.ts > segment.out
        ffprobe -loglevel warning -show_entries format=format_name,duration test.ts > test.out
        diff -u segment.out test.out
        wc -l test.out | grep 4 # sanity check output file length
        ffprobe segment.ts 2>&1 | grep 'service_provider: Livepeer Media Server'
    `
	run(cmd)

	// If format *and* mux opts specified, should prefer format opts
	tsFmt := out[0]
	mp4Fmt := out[0]
	if tsFmt.Muxer.Name != "hls" || mp4Fmt.Muxer.Name != "hls" {
		t.Error("Sanity check failed; expected non-empty muxer format names")
	}
	tsFmt.Oname = dir + "/actually_mpegts.flv"
	tsFmt.Profile.Format = FormatMPEGTS
	mp4Fmt.Oname = dir + "/actually_mp4.flv"
	mp4Fmt.Profile.Format = FormatMP4
	out = []TranscodeOptions{tsFmt, mp4Fmt}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        # mpegts should match original exactly.
        ffprobe -loglevel warning -show_entries format=format_name,duration actually_mpegts.flv > actually_mpegts.out

        # mp4 will be a bit different so construct manually
        ffprobe -loglevel warning -show_entries format=format_name,duration actually_mp4.flv > actually_mp4.out
		cat <<- EOF > expected_mp4.out
			[FORMAT]
			format_name=mov,mp4,m4a,3gp,3g2,mj2
			duration=8.032667
			[/FORMAT]
		EOF

        diff -u actually_mpegts.out test.out
        diff -u actually_mp4.out expected_mp4.out
    `
	run(cmd)

	// If format alone specified, use format regardless of file extension
	tsFmt.Muxer.Name = ""
	tsFmt.Oname = dir + "/actually_mpegts_2.flv"
	mp4Fmt.Muxer.Name = ""
	mp4Fmt.Oname = dir + "/actually_mp4_2.flv"
	out = []TranscodeOptions{tsFmt, mp4Fmt}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	cmd = `
        # mpegts should match original exactly.
        ffprobe -loglevel warning -show_entries format=format_name,duration actually_mpegts_2.flv > actually_mpegts_2.out

        # mp4 will be a bit different so construct manually
        ffprobe -loglevel warning -show_entries format=format_name,duration actually_mp4_2.flv > actually_mp4_2.out

        diff -u actually_mpegts_2.out test.out
        diff -u actually_mp4_2.out expected_mp4.out
    `
	run(cmd)

	// Check that the MP4 preset specifically has the moov atom at the beginning
	// Do this by checking for 'moov' within the first few bytes.

	// Also sanity check that a mp4 muxer by default does *not* contain moov
	// within the first few bytes
	out = []TranscodeOptions{{
		Oname:        dir + "/no_moov.mp4",
		VideoEncoder: ComponentOptions{Name: "copy"},
		AudioEncoder: ComponentOptions{Name: "copy"},
		Muxer:        ComponentOptions{Name: "mp4"},
	}}
	_, err = Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}

	cmd = `
        # '6d 6f 6f 76' being ascii for 'moov' ; search for that
        xxd -p -g 0 -c 256 -l 128 actually_mp4.flv | grep 6d6f6f76

        # sanity check no moov at beginning by default
        ffprobe -loglevel warning -show_format no_moov.mp4 | grep format_name=mov,mp4
        ( xxd -p -g 0 -c 256 -l 128 no_moov.mp4 | grep 6d6f6f76 || echo "no moov found" )  | grep "no moov found"
    `
	run(cmd)

	// If invalid format specified, should error out
	out[0].Profile.Format = -1
	_, err = Transcode3(in, out)
	if err != ErrTranscoderFmt {
		t.Error("Did not get expected error with invalid format ", err)
	}
}

func TestTranscoder_Metadata(t *testing.T) {
	runTestTranscoder_Metadata(t, Software)
}

func runTestTranscoder_Metadata(t *testing.T, accel Acceleration) {
	// check that metadata is there in all segments
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	err := RTMPToHLS("../transcoder/test.ts", dir+"/in.m3u8", dir+"/in_%d.ts", "2", 0)
	if err != nil {
		t.Error(err)
	}
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	for i := 0; i < 4; i++ {
		in := &TranscodeOptionsIn{
			Fname: fmt.Sprintf("%s/in_%d.ts", dir, i),
			Accel: accel,
		}
		out := []TranscodeOptions{{
			Accel:   accel,
			Oname:   fmt.Sprintf("%s/out_%d.ts", dir, i),
			Profile: P144p30fps16x9,
			Metadata: map[string]string{
				"service_name": fmt.Sprintf("lpms-test-%d", i),
			},
		}}
		_, err := tc.Transcode(in, out)
		if err != nil {
			t.Error(err)
		}
	}

	cmd := `
		ffprobe -hide_banner -i out_1.ts
		ffprobe -i out_0.ts 2>&1 | grep 'service_name    : lpms-test-0'
		ffprobe -i out_1.ts 2>&1 | grep 'service_name    : lpms-test-1'
		ffprobe -i out_2.ts 2>&1 | grep 'service_name    : lpms-test-2'
		ffprobe -i out_3.ts 2>&1 | grep 'service_name    : lpms-test-3'
	`
	run(cmd)
}

func TestTranscoder_IgnoreUnknown(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
        ffmpeg -i "$1/../transcoder/test.ts" -c copy -t 1 test.ts
        echo "data file" > data

        ffmpeg -loglevel verbose -i test.ts \
            -f  bin -i data  \
            -map 0:v:0 \
            -map 0:a:0 \
            -map 1:0 \
            -c copy \
            out.ts

        ffprobe -show_streams out.ts | grep codec_name > streams.out
        tee expected-streams.out <<-EOF
			codec_name=h264
			codec_name=aac
			codec_name=bin_data
			EOF
        diff -u expected-streams.out streams.out
    `
	run(cmd)
	in := &TranscodeOptionsIn{Fname: dir + "/out.ts"}
	out := []TranscodeOptions{{Oname: dir + "/transcoded.ts", Profile: P144p30fps16x9}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	// transcoded output should ignore the unknown streams
	cmd = `
        ffprobe -show_streams transcoded.ts | grep codec_name > transcoded-streams.out
        tee transcoded-expected-streams.out <<-EOF
			codec_name=h264
			codec_name=aac
			EOF
        diff -u transcoded-expected-streams.out transcoded-streams.out
    `
	run(cmd)
}

func TestTranscoder_GetCodecInfo(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	fname := path.Join(wd, "..", "data", "zero-frame.ts")
	status, format, err := GetCodecInfo(fname)
	isZeroFrame := status == CodecStatusNeedsBypass
	fmt.Printf("zero-frame.ts %t %s %s %d %v\n", isZeroFrame, format.Acodec, format.Vcodec, format.PixFormat, err)
	if isZeroFrame != true {
		t.Errorf("Expecting true, got %v fname=%s", isZeroFrame, fname)
	}
	data, err := ioutil.ReadFile(fname)
	if err != nil {
		t.Error(err)
	}
	status, format, err = GetCodecInfoBytes(data)
	isZeroFrame = status == CodecStatusNeedsBypass
	fmt.Printf("zero-frame.ts %t %s %s %d %v\n", isZeroFrame, format.Acodec, format.Vcodec, format.PixFormat, err)
	if err != nil {
		t.Error(err)
	}
	if isZeroFrame != true {
		t.Errorf("Expecting true, got %v fname=%s", isZeroFrame, fname)
	}
	_, err = HasZeroVideoFrameBytes(nil)
	if err != ErrEmptyData {
		t.Errorf("Unexpected error %v", err)
	}
	fname = path.Join(wd, "..", "data", "bunny.mp4")
	status, format, err = GetCodecInfo(fname)
	isZeroFrame = status == CodecStatusNeedsBypass
	fmt.Printf("bunny.mp4 %t %s %s %d %v\n", isZeroFrame, format.Acodec, format.Vcodec, format.PixFormat, err)
	if isZeroFrame != false {
		t.Errorf("Expecting false, got %v fname=%s", isZeroFrame, fname)
	}
	assert.Equal(t, "h264", format.Vcodec)
	assert.Equal(t, "aac", format.Acodec)
}

func TestTranscoder_ZeroFrameLongBadSegment(t *testing.T) {
	badSegment := make([]byte, 16*1024*1024)
	res, err := HasZeroVideoFrameBytes(badSegment)
	if err != nil {
		t.Error(err)
	}
	if res {
		t.Errorf("Expecting false, got %v", res)
	}
}

func TestTranscoder_Clip(t *testing.T) {
	_, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// If no format and no mux opts specified, should be based on file extension
	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	P144p30fps16x9 := P144p30fps16x9
	P144p30fps16x9.Framerate = 0
	out := []TranscodeOptions{{
		Profile: P144p30fps16x9,
		Oname:   dir + "/test_0.mp4",
		// Oname: "./test_0.mp4",
		From: time.Second,
		To:   3 * time.Second,
	}}
	res, err := Transcode3(in, out)
	require.NoError(t, err)
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 480, res.Decoded.Frames)
	assert.Equal(t, int64(442368000), res.Decoded.Pixels)
	assert.Equal(t, 120, res.Encoded[0].Frames)
	assert.Equal(t, int64(4423680), res.Encoded[0].Pixels)
}

func TestTranscoder_Clip2(t *testing.T) {
	_, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// If no format and no mux opts specified, should be based on file extension
	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	P144p30fps16x9 := P144p30fps16x9
	P144p30fps16x9.GOP = 5 * time.Second
	P144p30fps16x9.Framerate = 120
	out := []TranscodeOptions{{
		Profile: P144p30fps16x9,
		Oname:   dir + "/test_0.mp4",
		// Oname: "./test_1.mp4",
		From: time.Second,
		To:   6 * time.Second,
	}}
	res, err := Transcode3(in, out)
	require.NoError(t, err)
	if err != nil {
		t.Error(err)
	}
	assert.Equal(t, 480, res.Decoded.Frames)
	assert.Equal(t, int64(442368000), res.Decoded.Pixels)
	assert.Equal(t, 601, res.Encoded[0].Frames)
	assert.Equal(t, int64(22155264), res.Encoded[0].Pixels)
}

func TestTranscoder_VFR(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// prepare the input by generating a vfr video and verify its properties
	cmd := `
    ffmpeg -hide_banner -i "$1/../transcoder/test.ts" -an -vf "setpts='\
    if(eq(N, 0), 33373260,\
    if(eq(N, 1), 33375870,\
    if(eq(N, 2), 33379560,\
    if(eq(N, 3), 33381360,\
    if(eq(N, 4), 33384960,\
    if(eq(N, 5), 33387660,\
    if(eq(N, 6), 33391350,\
    if(eq(N, 7), 33394950,\
    if(eq(N, 8), 33397650,\
    if(eq(N, 9), 33400350,\
    if(eq(N, 10), 33403050,\
    if(eq(N, 11), 33405750,\
    if(eq(N, 12), 33408450,\
    if(eq(N, 13), 33412050,\
    if(eq(N, 14), 33414750,\
    if(eq(N, 15), 33418350,\
    if(eq(N, 16), 33421950,\
    if(eq(N, 17), 33423750,\
    if(eq(N, 18), 33426450,\
    if(eq(N, 19), 33430950,\
    if(eq(N, 20), 33435450,\
    if(eq(N, 21), 33437340,\
    if(eq(N, 22), 33440040,\
    if(eq(N, 23), 33441840,\
    if(eq(N, 24), 33445440,\
    if(eq(N, 25), 33449040,\
    if(eq(N, 26), 33451740,\
    if(eq(N, 27), 33455340,\
    if(eq(N, 28), 33458040,\
    if(eq(N, 29), 33459750, 0\
  ))))))))))))))))))))))))))))))',scale=320:240" -c:v libx264 -bf 0 -frames:v 30 -copyts -enc_time_base 1:90000 -fps_mode vfr -muxdelay 0 in.ts

    ffprobe -hide_banner -i in.ts -select_streams v:0 -show_entries packet=pts,duration -of csv=p=0 | sed '/^$/d' | sed 's/,*$//g' > input-pts.out

    # Double check that we've correctly generated the expected pts for input
    cat << PTS_EOF > expected-input-pts.out
33373260,N/A
33375870,N/A
33379560,N/A
33381360,N/A
33384960,N/A
33387660,N/A
33391350,N/A
33394950,N/A
33397650,N/A
33400350,N/A
33403050,N/A
33405750,N/A
33408450,N/A
33412050,N/A
33414750,N/A
33418350,N/A
33421950,N/A
33423750,N/A
33426450,N/A
33430950,N/A
33435450,N/A
33437340,N/A
33440040,N/A
33441840,N/A
33445440,N/A
33449040,N/A
33451740,N/A
33455340,N/A
33458040,N/A
33459750,N/A
PTS_EOF

 diff -u expected-input-pts.out input-pts.out
    `

	run(cmd)

	err := Transcode(
		dir+"/in.ts",
		dir, []VideoProfile{P240p30fps4x3})
	require.Nil(t, err)

	// check output
	cmd = `
    # reproduce expected lpms output using ffmpeg
    ffmpeg -debug_ts -loglevel trace -i in.ts -vf 'scale=136x240,fps=30/1:eof_action=pass' -c:v libx264 -copyts -muxdelay 0 out-ffmpeg.ts

    ffprobe -show_packets out-ffmpeg.ts | grep dts= > ffmpeg-dts.out
    ffprobe -show_packets out0in.ts | grep dts= > lpms-dts.out

    diff -u lpms-dts.out ffmpeg-dts.out
  `
	run(cmd)
}

func TestDurationFPS_GetCodecInfo(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	//Generate test files
	cmd := `
	cp "$1/../data/duplicate-audio-dts.ts" test.ts
	ffprobe -loglevel warning -show_format test.ts | grep duration=2.008555
	ffprobe -loglevel warning -show_streams -select_streams v test.ts | grep r_frame_rate=30/1
	cp "$1/../data/bunny.mp4" test.mp4
	ffmpeg -loglevel warning -i test.mp4 -c:v copy -c:a copy -t 2 test-short.mp4
	ffprobe -loglevel warning -show_format test-short.mp4 | grep duration=2.043356
	ffprobe -loglevel warning -show_streams -select_streams v test-short.mp4 | grep r_frame_rate=24/1
	ffmpeg -loglevel warning -i test-short.mp4 -c:v libvpx -c:a vorbis -strict -2 -t 2 test.webm
	ffprobe -loglevel warning -show_format test.webm | grep duration=2.049000
	ffprobe -loglevel warning -show_streams -select_streams v test.webm | grep r_frame_rate=24/1
	ffmpeg -loglevel warning -i test-short.mp4 -vn -c:a aac -b:a 128k test.m4a
	ffprobe -loglevel warning -show_format test.m4a | grep duration=2.042993
	ffmpeg -loglevel warning -i test-short.mp4 -vn -c:a flac test.flac
	ffprobe -loglevel warning -show_format test.flac | grep duration=2.043356

	ffmpeg -loglevel warning -i test.mp4 -vn -c:a copy stereo-audio.aac
	ffprobe -show_entries stream=channels,channel_layout -of csv stereo-audio.aac | grep stream,2,stereo
	ffprobe -show_format stereo-audio.aac | grep duration=52.440083

	ffmpeg -i test.mp4 -vn stereo-audio.wav
	ffprobe -show_format stereo-audio.wav | grep duration=60.139683

	cp $1/../data/audio.mp3 test.mp3
	ffprobe -show_format test.mp3 | grep duration=1.968000

	cp $1/../data/audio.ogg test.ogg
	ffprobe -show_format test.ogg | grep duration=1.974500
	`
	run(cmd)

	files := []struct {
		Filename string
		Format   string
		Duration int64
		FPS      float32

		// skip check if bytes version is known to fail duration
		BytesSkipDuration bool
	}{
		{Filename: "test-short.mp4", Format: "mov,mp4,m4a,3gp,3g2,mj2", Duration: 2, FPS: 24},
		{Filename: "test.ts", Format: "mpegts", Duration: 2, FPS: 30.0, BytesSkipDuration: true},
		{Filename: "test.flac", Format: "flac", Duration: 2},
		{Filename: "test.webm", Format: "matroska,webm", Duration: 2, FPS: 24},
		{Filename: "test.m4a", Format: "mov,mp4,m4a,3gp,3g2,mj2", Duration: 2},
		{Filename: "stereo-audio.aac", Format: "aac", Duration: 52},
		{Filename: "stereo-audio.wav", Format: "wav", Duration: 60},
		{Filename: "test.mp3", Format: "mp3", Duration: 1},
		{Filename: "test.ogg", Format: "ogg", Duration: 1, BytesSkipDuration: true},
	}
	for _, file := range files {
		t.Run(file.Filename, func(t *testing.T) {
			fname := path.Join(dir, file.Filename)
			// use 'bytes' prefix to prevent test runner regex matching
			for _, tt := range []string{"GetCodecInfo", "BytesGetCodecInfo"} {
				t.Run(tt, func(t *testing.T) {
					assert := assert.New(t)
					f := func() (CodecStatus, MediaFormatInfo, error) {
						if tt == "GetCodecInfo" {
							return GetCodecInfo(fname)
						}
						d, err := os.ReadFile(fname)
						assert.Nil(err, "reading file")
						return GetCodecInfoBytes(d)
					}
					status, format, err := f()
					assert.Nil(err, "getcodecinfo error")
					assert.Equal(CodecStatusOk, status, "status not ok")
					assert.Equal(file.Format, format.Format, "format mismatch")
					if tt == "BytesGetCodecInfo" && file.BytesSkipDuration {
						assert.Equal(int64(0), format.DurSecs, "special duration mismatch")
					} else {
						assert.Equal(file.Duration, format.DurSecs, "duration mismatch")
					}
					assert.Equal(file.FPS, format.FPS, "fps mismatch")
				})
			}
		})
	}
}

func TestTranscoder_Rotation(t *testing.T) {
	runRotationTests(t, Software)
	// TODO hevc
}

func runRotationTests(t *testing.T, accel Acceleration) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// generate a sample that is rotated mid-stream
	cmd := `
		ffmpeg -i "$1/../transcoder/test.ts" -an -c:v libx264 -g 120 -s 100x56 -f segment -t 6 test-%d.ts
		ffmpeg -i test-1.ts -vf transpose -c:v libx264 -c:a copy -copyts -muxdelay 0 test-1-transposed.ts
		ffprobe -select_streams v -show_entries format=start_time,duration:stream=width,height -of default=nw=1 test-1.ts > test-1.data
		ffprobe -select_streams v -count_frames -show_entries format=start_time,duration:stream=width,height,nb_read_frames -of default=nw=1 test-1-transposed.ts > test-1-transposed.data

		cat <<-EOF1 > test-1.expected
			width=100
			height=56
			width=100
			height=56
			start_time=3.433333
			duration=2.000000
		EOF1

		# transposed
		cat <<-EOF2 > test-1-transposed.expected
			width=56
			height=100
			nb_read_frames=120
			width=56
			height=100
			nb_read_frames=120
			start_time=3.433333
			duration=2.000000
		EOF2

		diff -u test-1.expected test-1.data
		diff -u test-1-transposed.expected test-1-transposed.data

		cat test-0.ts test-1-transposed.ts test-2.ts > double-rotated.ts
		cat test-0.ts test-1-transposed.ts > single-rotated.ts
	`
	run(cmd)

	profile := P144p30fps16x9
	profilePassthrough := profile
	profilePassthrough.Framerate = 0
	res, err := Transcode3(
		&TranscodeOptionsIn{Fname: dir + "/double-rotated.ts", Accel: accel},
		[]TranscodeOptions{{
			Profile: profile,
			Oname:   dir + "/out-double-rotated-30fps.ts",
			Accel:   accel,
		}, {
			Profile: profilePassthrough,
			Oname:   dir + "/out-double-rotated.ts",
			Accel:   accel,
		}})
	require.NoError(t, err)

	assert.Equal(t, 360, res.Decoded.Frames)
	assert.Equal(t, 181, res.Encoded[0].Frames) // should be 180 ... ts rounding ?
	assert.Equal(t, 360, res.Encoded[1].Frames)

	// TODO test rollover of gop interval during flush

	cmd = `
		ffprobe -count_frames -show_streams out-double-rotated.ts | grep nb_read_frames=360
		ffprobe -show_entries frame=height,width -of csv=p=0 out-double-rotated.ts | sed 's/,$//g' | uniq -c | sed 's/^ *//g' > out.dims
		ffprobe -show_entries frame=height,width -of csv=p=0 out-double-rotated-30fps.ts | sed 's/,$//g' | uniq -c | sed 's/^ *//g' > out-30fps.dims
	`

	// compare timestamps with input but software-only for now
	// nvidia timestamps differ by the first 2 and last 2 packets
	// TODO figure out why that is
	// TODO ideally check for this diff anyway w nvidia (so we know when / if it changes)
	if accel == Software {
		cmd = cmd + `
			ffprobe -show_entries packet=dts -of csv=p=0 out-double-rotated.ts | sed 's/,$//g' > out.ptsdts
			ffprobe -show_entries packet=dts -of csv=p=0 double-rotated.ts | sed 's/,$//g' > expected.ptsdts
			diff -u expected.ptsdts out.ptsdts
		`
	}

	cmd = cmd + `
			cat <<-EOF1 > expected.dims
				120 256,144
				120 146,260
				120 256,144
			EOF1

			cat <<-EOF2 > expected-30fps.dims
				60 256,144
				60 146,260
				61 256,144
			EOF2

		diff -u expected.dims out.dims
		diff -u expected-30fps.dims out-30fps.dims
	`

	run(cmd)

	// double check separate transcodes of portrait vs landscape
	_, err = Transcode3(
		&TranscodeOptionsIn{Fname: dir + "/test-1-transposed.ts", Accel: accel},
		[]TranscodeOptions{{
			Profile: profile,
			Oname:   dir + "/out-transposed-30fps.ts",
			Accel:   accel,
		}, {
			Profile: profilePassthrough,
			Oname:   dir + "/out-transposed.ts",
			Accel:   accel,
		}})
	require.NoError(t, err)

	// use the same transcoder instance for the landscape stuff
	tc := NewTranscoder()
	defer tc.StopTranscoder()
	_, err = tc.Transcode(&TranscodeOptionsIn{
		Fname: dir + "/test-0.ts", Accel: accel,
	}, []TranscodeOptions{{
		Profile: profile,
		Oname:   dir + "/out-test-0-30fps.ts",
		Accel:   accel,
	}, {
		Profile: profilePassthrough,
		Oname:   dir + "/out-test-0.ts",
		Accel:   accel,
	}})
	require.NoError(t, err)

	_, err = tc.Transcode(&TranscodeOptionsIn{
		Fname: dir + "/test-2.ts", Accel: accel,
	}, []TranscodeOptions{{
		Profile: profile,
		Oname:   dir + "/out-test-2-30fps.ts",
		Accel:   accel,
	}, {
		Profile: profilePassthrough,
		Oname:   dir + "/out-test-2.ts",
		Accel:   accel,
	}})
	require.NoError(t, err)

	cmd = `
		cat out-test-0.ts  out-transposed.ts out-test-2.ts > out-test-concat.ts
		ffprobe -show_entries frame=pts,pkt_dts,duration,pict_type,width,height -of csv out-test-concat.ts > out-test-concat.framedata

		cat out-test-0-30fps.ts  out-transposed-30fps.ts out-test-2-30fps.ts > out-test-concat-30fps.ts
		ffprobe -show_entries frame=pts,pkt_dts,duration,pict_type,width,height out-test-concat-30fps.ts -of csv > out-test-concat-30fps.framedata

		ffprobe -show_entries frame=pts,pkt_dts,duration,pict_type,width,height out-double-rotated.ts -of csv > out-double-rotated.framedata

		ffprobe -show_entries frame=pts,pkt_dts,duration,pict_type,width,height out-double-rotated-30fps.ts -of csv > out-double-rotated-30fps.framedata

		diff -u out-test-concat.framedata out-double-rotated.framedata

		# this does not line up
		#diff -u out-test-concat-30fps.framedata out-double-rotated-30fps.framedata
	`
	run(cmd)

	// check single rotations
	res, err = Transcode3(
		&TranscodeOptionsIn{Fname: dir + "/single-rotated.ts", Accel: accel},
		[]TranscodeOptions{{
			Profile: profile,
			Oname:   dir + "/out-single-rotated-30fps.ts",
			Accel:   accel,
		}, {
			Profile: profilePassthrough,
			Oname:   dir + "/out-single-rotated.ts",
			Accel:   accel,
		}})
	require.NoError(t, err)

	assert.Equal(t, 240, res.Decoded.Frames)
	assert.Equal(t, 121, res.Encoded[0].Frames) // should be 120 ... ts rounding ?
	assert.Equal(t, 240, res.Encoded[1].Frames)

	cmd = `
		ffprobe -count_frames -show_streams out-single-rotated.ts | grep nb_read_frames=24
		ffprobe -show_entries frame=height,width -of csv=p=0 out-single-rotated.ts | sed 's/,$//g' | uniq -c | sed 's/^ *//g' > single-out.dims
		ffprobe -show_entries frame=height,width -of csv=p=0 out-single-rotated-30fps.ts | sed 's/,$//g' | uniq -c | sed 's/^ *//g' > single-out-30fps.dims
	`

	cmd = cmd + `
			cat <<-EOF1 > single-expected.dims
				120 256,144
				120 146,260
			EOF1

			cat <<-EOF2 > single-expected-30fps.dims
				60 256,144
				61 146,260
			EOF2

		diff -u single-expected.dims single-out.dims
		diff -u single-expected-30fps.dims single-out-30fps.dims
	`
	run(cmd)
}

func TestTranscoder_DemuxerOpts(t *testing.T) {
	// generate test files: a few frames of raw video
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `
		# use an unusual pixel format
		ffmpeg -i "$1/../transcoder/test.ts" -an -c:v rawvideo -pix_fmt gbrp12be -s 320x240 -r 1 -frames:v 3 -f rawvideo test.raw
		ffprobe -show_streams -count_frames -pixel_format gbrp12be -video_size 320x240 -f rawvideo test.raw 2>&1 | grep nb_read_frames=3
	`
	run(cmd)
	res, err := Transcode3(
		&TranscodeOptionsIn{
			Fname: dir + "/test.raw",
			Demuxer: ComponentOptions{
				Name: "rawvideo",
				Opts: map[string]string{
					"fflags":       "+discardcorrupt+nobuffer",
					"pixel_format": "gbrp12be",
					"video_size":   "320x240",
				},
			},
		},
		[]TranscodeOptions{{
			Oname: dir + "/out-%d.png",
			Profile: VideoProfile{
				Name:       "-",
				Resolution: "200x150",
				Bitrate:    "10k",
			},
		}})
	assert.Nil(t, err, "transcoder returned error")
	assert := assert.New(t)
	// we transcode 3 but decode/encode 2 due to nobuffer
	assert.Equal(2, res.Decoded.Frames, "decoded frame count did  not match")
	assert.Equal(2, res.Encoded[0].Frames, "encoded frame count did not match")
	assert.Equal(int64(2*320*240), res.Decoded.Pixels, "decoded pixel count did not match")
	assert.Equal(int64(2*200*150), res.Encoded[0].Pixels, "encoded frame count did not match")
}

func TestTranscoder_DemuxerOptsError(t *testing.T) {

	// nonexistent demuxer
	_, err := Transcode3(&TranscodeOptionsIn{
		Fname: "../transcoder/test.ts",
		Demuxer: ComponentOptions{
			Name: "nonexistent",
		},
	}, nil)
	assert.Equal(t, "Demuxer not found", err.Error())

	// wrong demuxer
	_, err = Transcode3(&TranscodeOptionsIn{
		Fname: "../transcoder/test.ts",
		Demuxer: ComponentOptions{
			Name: "mp4",
		},
	}, nil)
	assert.Equal(t, "Invalid data found when processing input", err.Error())

}

func TestTranscoder_PNGDemuxerOpts(t *testing.T) {
	// we implicitly add demuxer opts to png input that has a framerate
	// so test those
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
		ffmpeg -i $1/../transcoder/test.ts -an -frames:v 3 test-%d.png
	`
	run(cmd)
	res, err := Transcode3(&TranscodeOptionsIn{
		Fname: dir + "/test-%d.png",
		Profile: VideoProfile{
			Framerate:    1,
			FramerateDen: 3,
		},
	}, []TranscodeOptions{{
		Profile: P144p30fps16x9,
		Oname:   "out.ts",
	}})
	assert.Nil(t, err)
	assert.Equal(t, 3, res.Decoded.Frames)
	assert.Equal(t, 180, res.Encoded[0].Frames)
}

func TestTranscode_DurationLimit(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)
	cmd := `
		ffmpeg -f lavfi -i color=c=blue:s=1280x720 -r 1 -frames:v 301 -c:v libx264 test-dur-bad.ts
		ffmpeg -f lavfi -i color=c=blue:s=1280x720 -r 1 -frames:v 300 -c:v libx264 test-dur-good.ts
	`
	run(cmd)

	// Create a transcoder instance
	transcoder := NewTranscoder()
	defer transcoder.StopTranscoder()

	// Set up transcode options
	badInput := &TranscodeOptionsIn{
		Fname: fmt.Sprintf("%v/test-dur-bad.ts", dir),
		Accel: Software,
	}

	goodInput := &TranscodeOptionsIn{
		Fname: fmt.Sprintf("%v/test-dur-good.ts", dir),
		Accel: Software,
	}

	profiles := []VideoProfile{
		{
			Name:       "test_profile",
			Resolution: "854x480",
			Bitrate:    "1000k",
		},
	}

	options := []TranscodeOptions{
		{
			Oname:   fmt.Sprintf("%s/out-test-dur.ts", dir),
			Profile: profiles[0],
			Accel:   Software,
		},
	}

	// transcode bad input
	_, errBadInput := transcoder.Transcode(badInput, options)

	// Check that the correct error was returned
	assert.Equal(t, ErrTranscoderDuration, errBadInput)

	// transcode good input
	_, errGoodInput := transcoder.Transcode(goodInput, options)

	// Check that the correct error was returned
	if errGoodInput != nil {
		t.Error(errGoodInput)
	}
}

func TestTranscoder_NoDurationLimitBytes(t *testing.T) {
	run, dir := setupTest(t)
	defer os.RemoveAll(dir)

	cmd := `ffmpeg -f lavfi -i color=c=blue:s=1280x720 -r 1 -frames:v 301 -c:v libx264 test-dur-bad.ts`
	run(cmd)
	// Create a transcoder instance
	transcoder := NewTranscoder()
	defer transcoder.StopTranscoder()

	ir, iw, err := os.Pipe()
	fname := fmt.Sprintf("%s/test-dur-bad.ts", dir)
	_, err = os.Stat(fname)
	if err != nil {
		t.Fatal(err)
		return
	}

	go func(iw *os.File) {
		defer iw.Close()
		f, _ := os.Open(fname)
		io.Copy(iw, f)
	}(iw)

	badInput := &TranscodeOptionsIn{
		Fname: fmt.Sprintf("pipe:%d", ir.Fd()),
		Accel: Software,
	}

	profiles := []VideoProfile{
		{
			Name:       "test_profile",
			Resolution: "854x480",
			Bitrate:    "1000k",
		},
	}

	options := []TranscodeOptions{
		{
			Oname:   fmt.Sprintf("%s/out-test-dur.ts", dir),
			Profile: profiles[0],
			Accel:   Software,
		},
	}

	// transcode bad input
	_, errBadInput := transcoder.Transcode(badInput, options)

	assert.Nil(t, errBadInput)
}
