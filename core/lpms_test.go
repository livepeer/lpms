package core

import (
	"testing"
	"os"
	"fmt"
	"github.com/livepeer/lpms/stream"
	"time"
	"github.com/livepeer/lpms/segmenter"
	"context"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/format"
	"github.com/nareix/joy4/av/avutil"
	"github.com/golang/glog"
	"io"
	"github.com/ericxtang/m3u8"
	"github.com/livepeer/lpms/vidplayer"
	"net/url"
	"github.com/nareix/joy4/format/rtmp"
)

type TestStream struct{}

func (s TestStream) String() string                       { return "" }
func (s *TestStream) GetStreamFormat() stream.VideoFormat { return stream.RTMP }
func (s *TestStream) GetStreamID() string                 { return "test" }
func (s *TestStream) Len() int64                          { return 0 }
func (s *TestStream) ReadRTMPFromStream(ctx context.Context, dst av.MuxCloser) (chan struct{}, error) {
	format.RegisterAll()
	wd, _ := os.Getwd()
	file, err := avutil.Open(wd + "/test.flv")
	if err != nil {
		fmt.Println("Error opening file: ", err)
		return nil, err
	}
	header, err := file.Streams()
	if err != nil {
		glog.Errorf("Error reading headers: %v", err)
		return nil, err
	}

	dst.WriteHeader(header)
	eof := make(chan struct{})
	go func(eof chan struct{}) {
		for {
			pkt, err := file.ReadPacket()
			if err == io.EOF {
				dst.WriteTrailer()
				eof <- struct{}{}
			}
			dst.WritePacket(pkt)
		}
	}(eof)
	return eof, nil
}
func (s *TestStream) WriteRTMPToStream(ctx context.Context, src av.DemuxCloser) (chan struct{}, error) {
	return nil, nil
}
func (s *TestStream) WriteHLSPlaylistToStream(pl m3u8.MediaPlaylist) error                { return nil }
func (s *TestStream) WriteHLSSegmentToStream(seg stream.HLSSegment) error                 { return nil }
func (s *TestStream) ReadHLSFromStream(ctx context.Context, buffer stream.HLSMuxer) error { return nil }
func (s *TestStream) ReadHLSSegment() (stream.HLSSegment, error)                          { return stream.HLSSegment{}, nil }
func (s *TestStream) Width() int                                                          { return 0 }
func (s *TestStream) Height() int                                                         { return 0 }
func (s *TestStream) Close()															  {}

func TestRetryRTMPToHLS(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Error("failed to get directory")
		t.Fail()
	}
	lpms := New(&LPMSOpts{WorkDir: fmt.Sprintf("%v/.tmp", dir)})

	strm := &TestStream{}
	strmUrl := fmt.Sprintf("rtmp://localhost:%v/stream/%v", "19355", strm.GetStreamID())
	hs := stream.NewBasicHLSVideoStream("test", 3)
	ctx, _ := context.WithTimeout(context.Background(), time.Second*5)
	opt := segmenter.SegmenterOptions{SegLength: 8 * time.Second}
	s := segmenter.NewFFMpegVideoSegmenter("tmp", hs.GetStreamID(), strmUrl, opt)

	server := &rtmp.Server{Addr: ":1935"}
	player := vidplayer.NewVidPlayer(server, "", nil)

	player.HandleRTMPPlay(
		func(url *url.URL) (stream.RTMPVideoStream, error) {
			return strm, nil
		})

	//Kick off RTMP server
	go func() {
		err := player.RtmpServer.ListenAndServe()
		if err != nil {
			t.Errorf("Error kicking off RTMP server: %v", err)
		}
	}()

	go func(s *segmenter.FFMpegVideoSegmenter){
		time.Sleep(200 * time.Millisecond)
		s.LocalRtmpUrl = "rtmp://localhost:1935"
	}(s)

	err = lpms.RTMPToHLS(s, ctx, true)

	if err != nil {
		t.Error("failed to connect")
		t.Fail()
	}
}