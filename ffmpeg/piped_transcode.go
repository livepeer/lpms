package ffmpeg

import (
	"fmt"
	"os"
)

type OutputReader struct {
	pipeEndpoint *os.File
}

func (r *OutputReader) Read(b []byte) (n int, err error) {
	return r.pipeEndpoint.Read(b)
}

func (r *OutputReader) Close() error {
	return r.pipeEndpoint.Close()
}

type PipedOutput struct {
	Read    *os.File
	Write   *os.File
	Options *TranscodeOptions
}

type PipedInput struct {
	Read    *os.File
	Write   *os.File
	Options *TranscodeOptionsIn
}

type PipedTranscoding struct {
	input   PipedInput
	outputs []PipedOutput
}

func (t *PipedTranscoding) SetInput(input TranscodeOptionsIn) error {
	t.input.Options = &input
	var err error
	t.input.Read, t.input.Write, err = os.Pipe()
	if err != nil {
		return err
	}
	// pass read pipe to ffmpeg
	t.input.Options.Fname = fmt.Sprintf("pipe:%d", t.input.Read.Fd())
	fmt.Printf("input name: %s;\n", t.input.Options.Fname)
	// keep write pipe for passing input chunks
	return nil
}

func (t *PipedTranscoding) SetOutputs(outputs []TranscodeOptions) error {
	t.outputs = make([]PipedOutput, len(outputs))
	for i, options := range outputs {
		t.outputs[i].Options = &TranscodeOptions{}
		*t.outputs[i].Options = options
		// Because output is pipe, format can't be deduced from file extension
		t.outputs[i].Options.Muxer = ComponentOptions{Name: "mpegts"}
		var err error
		t.outputs[i].Read, t.outputs[i].Write, err = os.Pipe()
		if err != nil {
			return err
		}
		// pass write pipe to ffmpeg
		t.outputs[i].Options.Oname = fmt.Sprintf("pipe:%d", t.outputs[i].Write.Fd())
		fmt.Printf("output %d name: %s;\n", i, t.outputs[i].Options.Oname)
		// keep read pipe for receiving output chunks
	}
	return nil
}

func (t *PipedTranscoding) Transcode() (*TranscodeResults, error) {
	out := make([]TranscodeOptions, len(t.outputs))
	for i := 0; i < len(t.outputs); i++ {
		out[i] = *t.outputs[i].Options
	}
	fmt.Printf("\n> Transcode3\n>in:%v\n>out:%v\n", t.input.Options, out)
	res, err := Transcode3(t.input.Options, out)
	return res, err
}

func (t *PipedTranscoding) Write(b []byte) (n int, err error) {
	return t.input.Write.Write(b)
}

func (t *PipedTranscoding) WriteClose() error {
	return t.input.Write.Close()
}

func (t *PipedTranscoding) GetOutputs() []OutputReader {
	readers := make([]OutputReader, len(t.outputs))
	for i := 0; i < len(t.outputs); i++ {
		readers[i].pipeEndpoint = t.outputs[i].Read
	}
	return readers
}
