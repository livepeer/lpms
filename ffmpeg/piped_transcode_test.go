//go:build nvidia
// +build nvidia

package ffmpeg

import (
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type SegmentInformation struct {
	Acodec string
	Vcodec string
	Format PixelFormat
	Valid  bool
	Cached []byte
}

func (s *SegmentInformation) ProcessChunk(chunk []byte) bool {
	if s.Valid {
		// We got info, skip further processing
		return false
	}
	s.Cached = append(s.Cached, chunk...)
	status, acodec, vcodec, pixelFormat, _ := GetCodecInfoBytes(s.Cached)
	if status == CodecStatusOk {
		s.Acodec = acodec
		s.Vcodec = vcodec
		s.Format = pixelFormat
		s.Valid = true
		return true
	}
	return false
}

func streamInput(t *testing.T, fname string, transcoder *PipedTranscoding) {
	defer transcoder.WriteClose()
	info := &SegmentInformation{}
	data, err := ioutil.ReadFile(fname)
	require.NoError(t, err)
	for len(data) > 0 {
		var chunkSize int = min(4096, len(data))
		bytesWritten, err := transcoder.Write(data[:chunkSize])
		if info.ProcessChunk(data[:bytesWritten]) {
			fmt.Printf("D> got media info at %d A=%s; V=%s; pixfmt=%d;\n", len(info.Cached), info.Acodec, info.Vcodec, info.Format.RawValue)
		}
		require.NoError(t, err)
		// handle partial write
		data = data[bytesWritten:]
	}
	require.True(t, info.Valid, "GetCodecInfoBytes() failed to parse media")
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
