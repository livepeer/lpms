package vidlistener

import (
	"fmt"
	"os/exec"
	"testing"
	"time"

	"github.com/livepeer/lpms/stream"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

func TestListener(t *testing.T) {
	server := &joy4rtmp.Server{Addr: ":1937"}
	listener := &VidListener{RtmpServer: server}
	listener.HandleRTMPPublish(
		func(reqPath string) (string, error) {
			return "test", nil
		},
		func(reqPath string) (stream.Stream, error) {
			// return errors.New("Some Error")
			return stream.NewVideoStream("test"), nil
		},
		func(reqPath string) {})

	ffmpegCmd := "ffmpeg"
	ffmpegArgs := []string{"-re", "-i", "../data/bunny2.mp4", "-c", "copy", "-f", "flv", "rtmp://localhost:1937/movie/stream"}

	cmd := exec.Command(ffmpegCmd, ffmpegArgs...)
	go cmd.Run()
	go listener.RtmpServer.ListenAndServe()

	time.Sleep(time.Second * 1)
	err := cmd.Process.Kill()
	if err != nil {
		fmt.Println("Error killing ffmpeg")
	}
}

// Integration test.
// func TestRTMPWithServer(t *testing.T) {
// 	server := &joy4rtmp.Server{Addr: ":1936"}
// 	listener := &VidListener{RtmpServer: server}
// 	listener.HandleRTMPPublish(
// 		func(reqPath string) (string, error) {
// 			return "teststream", nil
// 		},
// 		func(reqPath string) (*lpmsio.Stream, error) {
// 			header, err := demux.Streams()
// 			if err != nil {
// 				t.Fatal("Failed ot read stream header")
// 			}
// 			fmt.Println("header: ", header)

// 			counter := 0
// 			fmt.Println("data: ")
// 			for {
// 				packet, err := demux.ReadPacket()
// 				if err != nil {
// 					t.Fatal("Failed to read packets")
// 				}
// 				fmt.Print("\r", len(packet.Data))
// 				counter = counter + 1
// 			}
// 		},
// 		func(reqPath string) {})
// 	ffmpegCmd := "ffmpeg"
// 	ffmpegArgs := []string{"-re", "-i", "../data/bunny2.mp4", "-c", "copy", "-f", "flv", "rtmp://localhost:1936/movie/stream"}
// 	go exec.Command(ffmpegCmd, ffmpegArgs...).Run()

// 	go listener.RtmpServer.ListenAndServe()

// 	time.Sleep(time.Second * 1)
// 	if stream := listener.Streams["teststream"]; stream.StreamID != "teststream" {
// 		t.Fatal("Server did not set stream")
// 	}

// 	time.Sleep(time.Second * 1)
// }
