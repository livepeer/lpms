package ffmpeg

import (
	"io/ioutil"
	"os"
	"testing"
)

func Test_SignDataCreate(t *testing.T) {
	_, dir := setupTest(t)
	defer os.RemoveAll(dir)

	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	out := []TranscodeOptions{{
		Oname:        dir + "/signtest1.ts",
		Profile:      P720p60fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}, {
		Oname:        dir + "/signtest2.ts",
		Profile:      P360p30fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}, {
		Oname:        dir + "/signtest3.ts",
		Profile:      P360p30fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
	}}
	_, err := Transcode3(in, out)
	if err != nil {
		t.Error(err)
	}
	_, err = os.Stat(dir + "/signtest1.ts.bin")
	if os.IsNotExist(err) {
		t.Error(err)
	}
	_, err = os.Stat(dir + "/signtest2.ts.bin")
	if os.IsNotExist(err) {
		t.Error(err)
	}
	_, err = os.Stat(dir + "/signtest3.ts.bin")
	if os.IsNotExist(err) == false {
		t.Error(err)
	}

}
func Test_SignDataCompare(t *testing.T) {

	res, err := CompareSignatureByPath("../data/sign_sw1.bin", "../data/sign_nv1.bin")
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareSignatureByPath("../data/sign_sw2.bin", "../data/sign_nv2.bin")
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareSignatureByPath("../data/sign_sw1.bin", "../data/sign_sw2.bin")
	if err != nil || res != false {
		t.Error(err)
	}
	res, err = CompareSignatureByPath("../data/sign_nv1.bin", "../data/sign_nv2.bin")
	if err != nil || res != false {
		t.Error(err)
	}
	res, err = CompareSignatureByPath("../data/sign_sw1.bin", "../data/nodata.bin")
	if err == nil || res != false {
		t.Error(err)
	}

	//test CompareSignatureByBuffer function
	data0, err := ioutil.ReadFile("../data/sign_sw1.bin")
	if err != nil {
		t.Error(err)
	}
	data1, err := ioutil.ReadFile("../data/sign_sw2.bin")
	if err != nil {
		t.Error(err)
	}
	data2, err := ioutil.ReadFile("../data/sign_nv1.bin")
	if err != nil {
		t.Error(err)
	}
	res, err = CompareSignatureByBuffer(data0, data2)
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareSignatureByBuffer(data0, data1)
	if err != nil || res != false {
		t.Error(err)
	}
}
