package ffmpeg

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
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

func Test_SignUnescapedFilepath(t *testing.T) {
	_, dir := setupTest(t)
	defer os.RemoveAll(dir)

	// Test an output file name that contains special chars
	// like ":" and "\" that FFmpeg treats differently in AVOption
	oname, err := filepath.Abs(dir + "/out:720p\\test.ts")
	if err != nil {
		t.Error(err)
	}
	in := &TranscodeOptionsIn{Fname: "../transcoder/test.ts"}
	out := []TranscodeOptions{{
		Oname:        oname,
		Profile:      P720p60fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}}
	_, err = Transcode3(in, out)

	// Our transcoder module should correctly escape those characters
	// before passing them onto the signature filter
	if err != nil {
		t.Error(err)
	}
	_, err = os.Stat(oname + ".bin")
	if os.IsNotExist(err) {
		t.Error(err)
	}

	// Test same output file, but with a windows style absolute path
	// need to prefix it like /tmp/<dir>/ on unix systems, because without it
	// ffmpeg thinks it's a protocol called "C"
	out = []TranscodeOptions{{
		Oname:        dir + "/" + "C:\\Users\\test\\.lpData\\out720ptest.ts",
		Profile:      P720p60fps16x9,
		AudioEncoder: ComponentOptions{Name: "copy"},
		CalcSign:     true,
	}}
	_, err = Transcode3(in, out)

	if err != nil {
		t.Error(err)
	}
	_, err = os.Stat(oname + ".bin")
	if os.IsNotExist(err) {
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

	datax0 := data0[:289] // one FineSignature in file
	res, err = CompareSignatureByBuffer(datax0, data2)
	assert.False(t, res)
	assert.NoError(t, err)
	datax0 = data0[:279] // zero FineSignature in file
	res, err = CompareSignatureByBuffer(datax0, data2)
	assert.False(t, res)
	assert.Equal(t, ErrSignCompare, err)

	rand.Seed(time.Now().UnixNano())
	xdata0 := make([]byte, len(data0))
	xdata2 := make([]byte, len(data2))
	// check that CompareSignatureByBuffer does not segfault on random data
	for i := 0; i < 300; i++ {
		copy(xdata0, data0)
		copy(xdata2, data2)
		for j := 0; j < 20; j++ {
			pos := rand.Intn(len(xdata0))
			xdata0[pos] = byte(rand.Int31n(256))
			CompareSignatureByBuffer(xdata0, xdata2)
		}
		if i%100 == 0 {
			fmt.Printf("Processed %d times\n", i)
		}
	}
}

func getFileGroup(dir string, pattern string) []string {
	files, _ := ioutil.ReadDir(dir)
	var fileGroup []string
	for _, f := range files {
		if !strings.Contains(f.Name(), pattern) {
			continue
		}
		fileGroup = append(fileGroup, fmt.Sprintf(dir+"/"+f.Name()))
	}
	sort.Strings(fileGroup)
	return fileGroup
}

func createCartesianPairs(items []string) ([]string, []string) {
	var pairs1 []string
	var pairs2 []string
	for i, item1 := range items {
		for j := i + 1; j < len(items); j++ {
			pairs1 = append(pairs1, item1)
			pairs2 = append(pairs2, items[j])
		}
	}
	return pairs1, pairs2
}

func compareSignsPairwise(items1 []string, items2 []string, reportMode int) (float64, float64) {
	matchCount := 0.0
	misMatchCount := 0.0
	for i := 0; i < len(items1); i++ {
		signFile1 := items1[i]
		signFile2 := items2[i]
		res, _ := CompareSignatureByPath(signFile1, signFile2)
		if !res {
			misMatchCount++
		} else {
			matchCount++
		}
		if res && reportMode == 1 {
			fmt.Printf("Signature match: %s    %s\n", signFile1, signFile2)
		}
		if !res && reportMode == 2 {
			fmt.Printf("Signature mismatch: %s    %s\n", signFile1, signFile2)
		}
	}
	return matchCount, misMatchCount
}

func TestSignCompareClMetrics(t *testing.T) {
	// the goal is to ensure we are close to <1% false negative rate (false negative is reported signature mismatch for matching videos)
	// while also having a reasonably low false positive (signatures match for visually different videos) rate
	// comparison of rendition signatures, when renditions are encoded with different encoders (hardware vs software), is more likely to produce false negatives
	dir := "../data/bbb_signatures"

	// find CPU FP rate
	signs := getFileGroup(dir, "cpu_360")
	signs1, signs2 := createCartesianPairs(signs)
	cpuFp360, _ := compareSignsPairwise(signs1, signs2, 1)
	cpu360FpRatio := cpuFp360 / float64(len(signs1))

	// find CPU - Nvidia FN rate
	signs1 = getFileGroup(dir, "cpu_360")
	signs2 = getFileGroup(dir, "nv_360")
	_, cpuNv360Fn := compareSignsPairwise(signs1, signs2, 2)
	cpuNv360FnRatio := cpuNv360Fn / float64(len(signs1))

	// find CPU FP rate
	signs = getFileGroup(dir, "cpu_720")
	signs1, signs2 = createCartesianPairs(signs)
	cpu720Fp, _ := compareSignsPairwise(signs1, signs2, 1)
	cpu720FpRatio := cpu720Fp / float64(len(signs1))

	// find CPU - Nvidia FN rate
	signs1 = getFileGroup(dir, "cpu_720")
	signs2 = getFileGroup(dir, "nv_720")
	_, cpuNv720Fn := compareSignsPairwise(signs1, signs2, 2)
	cpuNv720FnRatio := cpuNv720Fn / float64(len(signs1))

	// Nvidia FP rate
	signs = getFileGroup(dir, "nv_720")
	signs1, signs2 = createCartesianPairs(signs)
	nv720Fp, _ := compareSignsPairwise(signs1, signs2, 1)
	nv720FpRatio := nv720Fp / float64(len(signs1))

	// Nvidia FP rate
	signs = getFileGroup(dir, "nv_360")
	signs1, signs2 = createCartesianPairs(signs)
	nv360Fp, _ := compareSignsPairwise(signs1, signs2, 1)
	nv360FpRatio := nv360Fp / float64(len(signs1))

	fmt.Printf("----------------\n")
	fmt.Printf("CPU 360p False Positive rate: %f\n", cpu360FpRatio)
	fmt.Printf("CPU 720p False Positive rate: %f\n", cpu720FpRatio)
	fmt.Printf("Nvidia 360p False Positive rate: %f\n", nv360FpRatio)
	fmt.Printf("Nvidia 720p False Positive rate: %f\n", nv720FpRatio)
	fmt.Printf("CPU - Nvidia 360p False Negative rate: %f\n", cpuNv360FnRatio)
	fmt.Printf("CPU - Nvidia 720p False Negative rate: %f\n", cpuNv720FnRatio)

	assert.True(t, cpuNv720FnRatio <= 0.01)
	assert.True(t, cpuNv360FnRatio <= 0.01)

	assert.True(t, cpu360FpRatio <= 0.15)
	assert.True(t, cpu720FpRatio <= 0.15)
	assert.True(t, nv720FpRatio <= 0.15)
	assert.True(t, nv720FpRatio <= 0.15)
}
