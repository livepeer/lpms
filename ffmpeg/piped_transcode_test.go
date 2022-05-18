//go:build nvidia
// +build nvidia

package ffmpeg

import (
	"io"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func streamInput(t *testing.T, fname string, transcoder *PipedTranscoding) {
	defer transcoder.WriteClose()
	data, err := ioutil.ReadFile(fname)
	require.NoError(t, err)
	// partial write is possible in case of socket, unusual with pipe
	for len(data) > 0 {
		bytesWritten, err := transcoder.Write(data)
		require.NoError(t, err)
		// handle partial write
		data = data[bytesWritten:]
	}
}

func streamOutput(t *testing.T, fname string, reader *OutputReader) {
	defer reader.Close()
	file, err := os.Create(fname)
	require.NoError(t, err)
	defer file.Close()
	buffer := make([]byte, 4096)
	for {
		byteCount, err := reader.Read(buffer)
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		bytesWritten, err := file.Write(buffer[:byteCount])
		require.Equal(t, byteCount, bytesWritten, "partial write to file")
	}
}

func TestTranscoder_Pipe(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	outNames := []string{
		path.Join(wd, "test-out-360.ts"),
		path.Join(wd, "test-out-240.ts"),
		path.Join(wd, "test-out-144.ts"),
	}
	inputFileName := path.Join(wd, "..", "samples", "sample_0_409_17041.ts")

	hevc := VideoProfile{Name: "P240p30fps16x9", Bitrate: "600k", Framerate: 30, AspectRatio: "16:9", Resolution: "426x240", Encoder: H265}
	transcoder := &PipedTranscoding{}
	transcoder.SetInput(TranscodeOptionsIn{Accel: Nvidia})
	transcoder.SetOutputs([]TranscodeOptions{
		{Profile: P360p30fps16x9, Accel: Nvidia},
		{Profile: hevc, Accel: Nvidia},
		{Profile: P144p30fps16x9, Accel: Nvidia},
	})
	// stream input chunks
	go streamInput(t, inputFileName, transcoder)
	// read output streams
	outputs := transcoder.GetOutputs()
	for i := 0; i < len(outputs); i++ {
		go streamOutput(t, outNames[i], &outputs[i])
	}
	// start transcode
	res, err := transcoder.Transcode()
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		// actual frame count should be 409 - is this a bug?
		assert.Equal(t, res.Encoded[i].Frames, 512, "must produce 512 frame in output %d", i)
	}
}
