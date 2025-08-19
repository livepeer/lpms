package ffmpeg

import (
	"os"
	"testing"
	"time"
)

// Tests fix for VFR inputs causing infinite frame duplication, resulting in huge output files.
func TestTimestampRegression(t *testing.T) {
	InitFFmpeg()

	inputFile := "../data/negative-timestamps.ts"
	if _, err := os.Stat(inputFile); err != nil {
		t.Skip("Problematic input file not available")
	}

	// Test the exact pattern that triggered infinite loops:
	// passthrough FPS followed by 30fps conversion
	passthroughProfile := P240p30fps16x9
	passthroughProfile.Framerate = 0 // passthrough

	options := []TranscodeOptions{
		{
			Oname:        "test_passthrough.mp4",
			Accel:        Software,
			Profile:      passthroughProfile,
			AudioEncoder: ComponentOptions{Name: "drop"},
		},
		{
			Oname:        "test_30fps.mp4",
			Accel:        Software,
			Profile:      P240p30fps16x9,
			AudioEncoder: ComponentOptions{Name: "drop"},
		},
	}

	// Should complete quickly with fix, would hang without it
	done := make(chan error, 1)
	var result *TranscodeResults

	go func() {
		var err error
		result, err = Transcode3(&TranscodeOptionsIn{
			Fname: inputFile,
			Accel: Software,
		}, options)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			// Size limit error means protection worked
			if err == ErrTranscoderOutputSize {
				t.Log("Size limit triggered")
				return
			}
			t.Fatal(err)
		}

		if result == nil {
			t.Fatal("No result")
		}

		// Verify outputs are reasonably sized (not 20-190GB)
		for _, opt := range options {
			if stat, err := os.Stat(opt.Oname); err == nil {
				size := stat.Size()
				if size > 10*1024*1024 { // 10MB is suspicious for this input
					t.Errorf("%s too large: %d bytes", opt.Oname, size)
				}
				os.Remove(opt.Oname)
			}
		}

	case <-time.After(30 * time.Second):
		t.Fatal("Hung for 30s - infinite loop detected")
	}
}
