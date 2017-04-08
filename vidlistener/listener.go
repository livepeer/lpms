package vidlistener

import (
	"context"
	"os"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/segmenter"
	"github.com/livepeer/lpms/stream"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

var segOptions = segmenter.SegmenterOptions{SegLength: time.Second * 2}

type LocalStream struct {
	StreamID  string
	Timestamp int64
}

type VidListener struct {
	RtmpServer *joy4rtmp.Server
}

//HandleRTMPPublish immediately turns the RTMP stream into segmented HLS, and writes it into a stream.
//It exposes getStreamID so the user can name the stream, and getStream so the user can keep track of all the streams.
func (self *VidListener) HandleRTMPPublish(
	getStreamID func(reqPath string) (string, error),
	getStream func(reqPath string) (stream.Stream, stream.Stream, error),
	endStream func(reqPath string)) error {

	self.RtmpServer.HandlePublish = func(conn *joy4rtmp.Conn) {
		glog.Infof("RTMP server got upstream")

		_, err := getStreamID(conn.URL.Path)
		if err != nil {
			glog.Errorf("RTMP Stream Publish Error: %v", err)
			return
		}

		rs, hs, err := getStream(conn.URL.Path)
		if err != nil {
			glog.Errorf("RTMP Publish couldn't get a destination stream for %v", conn.URL.Path)
			return
		}

		glog.Infof("Got RTMP Stream: %v", rs.GetStreamID())
		cew := make(chan error, 0)
		cs := make(chan error, 0)
		ctx, cancel := context.WithCancel(context.Background())
		glog.Infof("Writing RTMP to stream")
		go func() { cew <- rs.WriteRTMPToStream(ctx, conn) }()
		go func() { cs <- self.segmentStream(ctx, rs, hs) }()

		select {
		case err := <-cew:
			endStream(conn.URL.Path)
			glog.Infof("Final stream length: %v", rs.Len())
			glog.Error("Got error writing RTMP: ", err)
			cancel()
		case err := <-cs:
			glog.Errorf("Error segmenting, %v", err)
			cancel()
		}

	}
	return nil
}

func (self *VidListener) segmentStream(ctx context.Context, rs stream.Stream, hs stream.Stream) error {
	// //Invoke Segmenter
	workDir, _ := os.Getwd()
	workDir = workDir + "/tmp"
	localRtmpUrl := "rtmp://localhost" + self.RtmpServer.Addr + "/stream/" + rs.GetStreamID()
	s := segmenter.NewFFMpegVideoSegmenter(workDir, rs.GetStreamID(), localRtmpUrl, segOptions.SegLength)
	c := make(chan error, 1)
	go func() { c <- s.RTMPToHLS(ctx, segOptions) }()

	go func() {
		c <- func() error {
			for {
				pl, err := s.PollPlaylist(ctx)
				if err != nil {
					glog.Errorf("Got error polling playlist: %v", err)
					return err
				}
				// glog.Infof("Writing pl: %v", pl)
				hs.WriteHLSPlaylistToStream(*pl.Data)
			}
		}()
	}()

	go func() {
		c <- func() error {
			for {
				seg, err := s.PollSegment(ctx)
				if err != nil {
					return err
				}
				ss := stream.HLSSegment{Data: seg.Data, Name: seg.Name}
				glog.Infof("Writing stream: %v, len:%v", ss.Name, len(seg.Data))
				hs.WriteHLSSegmentToStream(ss)
			}
		}()
	}()

	select {
	case err := <-c:
		glog.Errorf("Error segmenting stream: %v", err)
		return err
	}
}
