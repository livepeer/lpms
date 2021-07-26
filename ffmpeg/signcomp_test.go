package ffmpeg

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"testing"
)

func TestCompareSignData(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	var device = []string{"sw", "nv"}
	fnames := make([]string, 0)
	for _, dev := range device {
		for i := 1; i <= 2; i++ {
			vname := fmt.Sprintf("sign_%s%d.bin", dev, i)
			fname := path.Join(wd, "..", "data", vname)
			fnames = append(fnames, fname)
		}
	}

	fnames = append(fnames, path.Join(wd, "..", "data", "nodata.bin"))

	res, err := CompareSignatureByPath(fnames[0], fnames[2])
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareSignatureByPath(fnames[1], fnames[3])
	if err != nil || res != true {
		t.Error(err)
	}
	res, err = CompareSignatureByPath(fnames[0], fnames[1])
	if err != nil || res != false {
		t.Error(err)
	}
	res, err = CompareSignatureByPath(fnames[2], fnames[3])
	if err != nil || res != false {
		t.Error(err)
	}
	res, err = CompareSignatureByPath(fnames[0], fnames[4])
	if err == nil || res != false {
		t.Error(err)
	}

	//test CompareSignatureByBuffer function
	data0, err := ioutil.ReadFile(fnames[0])
	if err != nil {
		t.Error(err)
	}
	data1, err := ioutil.ReadFile(fnames[1])
	if err != nil {
		t.Error(err)
	}
	data2, err := ioutil.ReadFile(fnames[2])
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
