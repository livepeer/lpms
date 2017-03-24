package vidlistener

import (
	"context"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/stream"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

type LocalStream struct {
	StreamID  string
	Timestamp int64
}

type VidListener struct {
	RtmpServer *joy4rtmp.Server
}

//HandleRTMPPublish writes the published RTMP stream into a stream.  It exposes getStreamID so the
//user can name the stream, and getStream so the user can keep track of all the streams.
func (s *VidListener) HandleRTMPPublish(
	getStreamID func(reqPath string) (string, error),
	getStream func(reqPath string) (*stream.Stream, error),
	endStream func(reqPath string)) error {

	s.RtmpServer.HandlePublish = func(conn *joy4rtmp.Conn) {
		glog.Infof("RTMP server got upstream")

		streamID, err := getStreamID(conn.URL.Path)
		if err != nil {
			glog.Errorf("RTMP Stream Publish Error: %v", err)
			return
		}

		stream, err := getStream(conn.URL.Path)
		if err != nil {
			glog.Errorf("RTMP Publish couldn't get a destination stream for %v", conn.URL.Path)
			return
		}

		glog.Infof("Got RTMP Stream: %v", streamID)
		c := make(chan error, 0)
		go func() { c <- stream.WriteRTMPToStream(context.Background(), conn) }()
		select {
		case err := <-c:
			endStream(conn.URL.Path)
			glog.Error("Got error writing RTMP: ", err)
		}

	}
	return nil
}
