package vidplayer

import (
	"context"
	"fmt"
	"testing"

	"time"

	"github.com/kz26/m3u8"
	"github.com/livepeer/lpms/stream"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

func TestRTMP(t *testing.T) {
	server := &joy4rtmp.Server{Addr: ":1936"}
	player := &VidPlayer{RtmpServer: server}
	var demuxer av.Demuxer
	gotUpvid := false
	gotPlayvid := false
	player.RtmpServer.HandlePublish = func(conn *joy4rtmp.Conn) {
		gotUpvid = true
		demuxer = conn
	}

	player.HandleRTMPPlay(func(ctx context.Context, reqPath string, dst av.MuxCloser) error {
		gotPlayvid = true
		fmt.Println(reqPath)
		avutil.CopyFile(dst, demuxer)
		return nil
	})

	// go server.ListenAndServe()

	// ffmpegCmd := "ffmpeg"
	// ffmpegArgs := []string{"-re", "-i", "../data/bunny2.mp4", "-c", "copy", "-f", "flv", "rtmp://localhost:1936/movie/stream"}
	// go exec.Command(ffmpegCmd, ffmpegArgs...).Run()

	// time.Sleep(time.Second * 1)

	// if gotUpvid == false {
	// 	t.Fatal("Didn't get the upstream video")
	// }

	// ffplayCmd := "ffplay"
	// ffplayArgs := []string{"rtmp://localhost:1936/movie/stream"}
	// go exec.Command(ffplayCmd, ffplayArgs...).Run()

	// time.Sleep(time.Second * 1)
	// if gotPlayvid == false {
	// 	t.Fatal("Didn't get the downstream video")
	// }
}

func TestHLS(t *testing.T) {
	player := &VidPlayer{}
	s := stream.NewStream("test")
	s.HLSTimeout = time.Second * 5
	//Write some packets into the stream
	s.WriteHLSPlaylistToStream(m3u8.MediaPlaylist{})
	s.WriteHLSSegmentToStream(stream.HLSSegment{})
	var buffer *stream.HLSBuffer
	player.HandleHLSPlay(func(reqPath string) (*stream.HLSBuffer, error) {
		//if can't find local cache, start downloading, and store in cache.
		if buffer == nil {
			buffer := stream.NewHLSBuffer()
			ec := make(chan error, 1)
			go func() { ec <- s.ReadHLSFromStream(buffer) }()
			// select {
			// case err := <-ec:
			// 	return err
			// }
		}
		return buffer, nil

		// if strings.HasSuffix(reqPath, ".m3u8") {
		// 	pl, err := buffer.WaitAndPopPlaylist(ctx)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	_, err = writer.Write(pl.Encode().Bytes())
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	return nil, nil
		// }

		// if strings.HasSuffix(reqPath, ".ts") {
		// 	pathArr := strings.Split(reqPath, "/")
		// 	segName := pathArr[len(pathArr)-1]
		// 	seg, err := buffer.WaitAndPopSegment(ctx, segName)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// 	_, err = writer.Write(seg)
		// 	if err != nil {
		// 		return nil, err
		// 	}
		// }

		// return nil, lpmsio.ErrNotFound
	})

	// go http.ListenAndServe(":8000", nil)

	//TODO: Add tests for checking if packets were written, etc.
}
