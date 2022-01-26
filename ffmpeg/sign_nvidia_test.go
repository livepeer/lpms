//go:build nvidia
// +build nvidia

package ffmpeg

import (
	"os"
	"testing"
)

func TestNvidia_SignDataCreate(t *testing.T) {
	_, dir := setupTest(t)

	filesMustExist := func(names []string) {
		for _, name := range names {
			_, err := os.Stat(dir + name)
			if os.IsNotExist(err) {
				t.Error(err)
			}
		}
	}
	compareSignatures := func(firstName, secondName string) {
		res, err := CompareSignatureByPath(dir+firstName, dir+secondName)
		if err != nil || res != true {
			t.Error(err)
		}
	}

	defer os.RemoveAll(dir)

	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	out := []TranscodeOptions{{
		Oname:        dir + "/cpu_signtest1.ts",
		Profile:      P720p60fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}, {
		Oname:        dir + "/cpu_signtest2.ts",
		Profile:      P360p30fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}, {
		Oname:        dir + "/nvidia_signtest1.ts",
		Profile:      P720p60fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
		Accel:        Nvidia,
	}, {
		Oname:        dir + "/nvidia_signtest2.ts",
		Profile:      P360p30fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
		Accel:        Nvidia,
	}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	filesMustExist([]string{
		"/cpu_signtest1.ts.bin",
		"/cpu_signtest2.ts.bin",
		"/nvidia_signtest1.ts.bin",
		"/nvidia_signtest2.ts.bin",
	})
	compareSignatures("/cpu_signtest1.ts.bin", "/nvidia_signtest1.ts.bin")
	compareSignatures("/cpu_signtest2.ts.bin", "/nvidia_signtest2.ts.bin")
}
