package main

import (
	"fmt"
	"io/ioutil"
	"math/rand"
	"time"

	"github.com/livepeer/lpms/ffmpeg"
)

func check2(data0, data2 []byte) {
	xdata0 := make([]byte, len(data0))
	xdata2 := make([]byte, len(data2))
	for i := 0; i < 300000000; i++ {
		copy(xdata0, data0)
		copy(xdata2, data2)
		for j := 0; j < 20; j++ {
			pos := rand.Intn(len(xdata0))
			xdata0[pos] = byte(rand.Int31n(256))
			ffmpeg.CompareSignatureByBuffer(xdata0, xdata2)
		}
		if i%100 == 0 {
			fmt.Printf("Processed %d times\n", i)
		}
	}
}

func main() {
	rand.Seed(time.Now().UnixNano())
	ffmpeg.InitFFmpegWithLogLevel(ffmpeg.FFLogInfo)
	ffmpeg.InitFFmpegWithLogLevel(ffmpeg.FFLogTrace)

	data0, err := ioutil.ReadFile("./data/sign_sw1.bin")
	if err != nil {
		panic(err)
	}
	data2, err := ioutil.ReadFile("./data/sign_nv1.bin")
	if err != nil {
		panic(err)
	}
	// data0 = data0[:289] // one FineSignature in file
	// data0 = data0[:279] // zero FineSignature in file
	res, err := ffmpeg.CompareSignatureByBuffer(data0, data2)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Compare res %v\n", res)
	check2(data0, data2)
	/*
		return
			datax0 := data0
			for i := 0; i < 300000000; i++ {
				datax0 = data0
				newLen := rand.Intn(len(datax0)-10) + 10
				datax0 = datax0[:newLen]
				rand.Read(datax0)
				ffmpeg.CompareSignatureByBuffer(datax0, data2)
				if i%100 == 0 {
					fmt.Printf("Processed %d times\n", i)
				}
			}
	*/
}
