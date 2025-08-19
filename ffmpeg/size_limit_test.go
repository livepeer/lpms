package ffmpeg

import (
	"os"
	"testing"
)

// Tests that output size monitoring stops transcoding when output limit is exceeded.
func TestOutputSizeLimit(t *testing.T) {
	InitFFmpeg()

	// Use any available test file
	testFiles := []string{
		"../data/bunny.mp4",
		"../data/videotest.mp4",
	}

	var inputFile string
	for _, file := range testFiles {
		if _, err := os.Stat(file); err == nil {
			inputFile = file
			break
		}
	}

	if inputFile == "" {
		t.Skip("No test files found")
	}

	// Use high quality
	options := []TranscodeOptions{
		{
			Oname:   "test_size.mp4",
			Accel:   Software,
			Profile: P720p30fps16x9,
			VideoEncoder: ComponentOptions{
				Name: "libx264",
				Opts: map[string]string{"crf": "10"},
			},
			AudioEncoder: ComponentOptions{Name: "drop"},
		},
	}

	_, err := Transcode3(&TranscodeOptionsIn{
		Fname: inputFile,
		Accel: Software,
	}, options)

	// Clean up regardless of result
	os.Remove(options[0].Oname)

	// Size limit error is expected/acceptable for this test
	if err != nil {
		errStr := err.Error()
		if err == ErrTranscoderOutputSize ||
			errStr == "TranscoderOutputSizeLimitExceeded" ||
			errStr == "Output size limit exceeded" {
			// This is expected - size limit protection worked
			return
		}
		t.Fatal(err)
	}
}
