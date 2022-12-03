//go:build nvidia
// +build nvidia

package ffmpeg

import (
	"fmt"
	"github.com/stretchr/testify/assert"
	"io/ioutil"
	"os"
	"sort"
	"strings"
	"testing"
)

const SignCompareMaxFalseNegativeRate = 0.01;
const SignCompareMaxFalsePositiveRate = 0.15;

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

func getFileGroup(dir string, pattern string, ext string) []string {
	files, _ := ioutil.ReadDir(dir)
	var fileGroup []string
	for _, f := range files {
		if !strings.Contains(f.Name(), pattern) || !strings.HasSuffix(f.Name(), ext) {
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

func TestNvidia_SignCompareClMetrics(t *testing.T) {
	// the goal is to ensure we are close to <1% false negative rate (false negative is reported signature mismatch for matching videos)
	// while also having a reasonably low false positive (signatures match for visually different videos) rate
	// comparison of rendition signatures, when renditions are encoded with different encoders (hardware vs software), is more likely to produce false negatives
	assert := assert.New(t)
	run, workDir := setupTest(t)
	defer os.RemoveAll(workDir)

	hlsDir := fmt.Sprintf("%s/%s", workDir, "hls")
	signDir := fmt.Sprintf("%s/%s", workDir, "signs")
	_ = os.Mkdir(hlsDir, 0700)
	_ = os.Mkdir(signDir, 0700)

	cmd := `
    # generate 2 sec segments
    cp "$1/../data/bunny2.mp4" .
    ffmpeg -loglevel warning -i bunny2.mp4 -c copy -f hls -hls_time 2 hls/source.m3u8
  `
	assert.True(run(cmd))

	// transcode on Nvidia and CPU to generate signatures
	files, _ := ioutil.ReadDir(hlsDir)
	for _, f := range files {
		if !strings.Contains(f.Name(), ".ts") {
			continue
		}
		in := &TranscodeOptionsIn{Fname: hlsDir + "/" + f.Name()}
		out := []TranscodeOptions{{
			Oname:        fmt.Sprintf("%s/cpu_720_%s", signDir, f.Name()),
			Profile:      P720p60fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
			CalcSign:     true,
		}, {
			Oname:        fmt.Sprintf("%s/cpu_360_%s", signDir, f.Name()),
			Profile:      P360p30fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
			CalcSign:     true,
		}, {
			Oname:        fmt.Sprintf("%s/nv_720_%s", signDir, f.Name()),
			Profile:      P720p60fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
			CalcSign:     true,
			Accel:        Nvidia,
		}, {
			Oname:        fmt.Sprintf("%s/nv_360_%s", signDir, f.Name()),
			Profile:      P360p30fps16x9,
			AudioEncoder: ComponentOptions{Name: "copy"},
			CalcSign:     true,
			Accel:        Nvidia,
		}}
		_, err := Transcode3(in, out)
		if err != nil {
			t.Error(err)
		}
	}

	// find CPU FP rate
	signs := getFileGroup(signDir, "cpu_360", ".bin")
	signs1, signs2 := createCartesianPairs(signs)
	cpuFp360, _ := compareSignsPairwise(signs1, signs2, 1)
	cpu360FpRatio := cpuFp360 / float64(len(signs1))

	// find CPU - Nvidia FN rate
	signs1 = getFileGroup(signDir, "cpu_360", ".bin")
	signs2 = getFileGroup(signDir, "nv_360", ".bin")
	_, cpuNv360Fn := compareSignsPairwise(signs1, signs2, 2)
	cpuNv360FnRatio := cpuNv360Fn / float64(len(signs1))

	// find CPU FP rate
	signs = getFileGroup(signDir, "cpu_720", ".bin")
	signs1, signs2 = createCartesianPairs(signs)
	cpu720Fp, _ := compareSignsPairwise(signs1, signs2, 1)
	cpu720FpRatio := cpu720Fp / float64(len(signs1))

	// find CPU - Nvidia FN rate
	signs1 = getFileGroup(signDir, "cpu_720", ".bin")
	signs2 = getFileGroup(signDir, "nv_720", ".bin")
	_, cpuNv720Fn := compareSignsPairwise(signs1, signs2, 2)
	cpuNv720FnRatio := cpuNv720Fn / float64(len(signs1))

	// Nvidia FP rate
	signs = getFileGroup(signDir, "nv_720", ".bin")
	signs1, signs2 = createCartesianPairs(signs)
	nv720Fp, _ := compareSignsPairwise(signs1, signs2, 1)
	nv720FpRatio := nv720Fp / float64(len(signs1))

	// Nvidia FP rate
	signs = getFileGroup(signDir, "nv_360", ".bin")
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

	assert.True(cpuNv720FnRatio <= SignCompareMaxFalseNegativeRate)
	assert.True(cpuNv360FnRatio <= SignCompareMaxFalseNegativeRate)

	assert.True(cpu360FpRatio <= SignCompareMaxFalsePositiveRate)
	assert.True(cpu720FpRatio <= SignCompareMaxFalsePositiveRate)
	assert.True(nv720FpRatio <= SignCompareMaxFalsePositiveRate)
	assert.True(nv720FpRatio <= SignCompareMaxFalsePositiveRate)
}
