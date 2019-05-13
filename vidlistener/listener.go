package vidlistener

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"net/http"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/segmenter"
	"github.com/livepeer/lpms/stream"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"

	"github.com/ericxtang/m3u8"
)

var segOptions = segmenter.SegmenterOptions{SegLength: time.Second * 2}

type LocalStream struct {
	StreamID  string
	Timestamp int64
}

type VidListener struct {
	RtmpServer *joy4rtmp.Server
}

//HandleRTMPPublish takes 3 parameters - makeStreamID, gotStream, and endStream.
//makeStreamID is called when the stream starts. It should return a streamID from the requestURL.
//gotStream is called when the stream starts.  It gives you access to the stream.
//endStream is called when the stream ends.  It gives you access to the stream.
func (self *VidListener) HandleRTMPPublish(
	makeStreamID func(url *url.URL) (strmID string),
	gotStream func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error,
	endStream func(url *url.URL, rtmpStrm stream.RTMPVideoStream) error) {

	if self.RtmpServer != nil {
		self.RtmpServer.HandlePublish = func(conn *joy4rtmp.Conn) {
			glog.V(2).Infof("RTMP server got upstream: %v", conn.URL)

			strmID := makeStreamID(conn.URL)
			if strmID == "" {
				conn.Close()
				return
			}
			s := stream.NewBasicRTMPVideoStream(strmID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			eof, err := s.WriteRTMPToStream(ctx, conn)
			if err != nil {
				return
			}

			err = gotStream(conn.URL, s)
			defer func() {
				endStream(conn.URL, s)
				conn.Close()
			}()
			if err != nil {
				glog.Errorf("Error RTMP gotStream handler: %v", err)
				return
			}

			select {
			case <-eof:
				// log something
			}
		}

	}
}

//HandleHLSPublish takes 3 parameters - makeStreamID, gotStream, and endStream.
//makeStreamID is called when the stream starts. It should return a streamID from the requestURL.
//gotStream is called when the stream starts.  It gives you access to the stream.
//endStream is called when the stream ends.  It gives you access to the stream.
func (self *VidListener) HandleHLSPublish(
	plURL *url.URL,
	makeStreamID func(plURL *url.URL) (strmID string),
	gotStream func(plURL *url.URL, hlsStrm stream.HLSVideoStream) error, // TODO: create this obj
	endStream func(plURL *url.URL, hlsStrm stream.HLSVideoStream) error) {

	if self.HlsServer != nil {
		// change to hLS
		self.HlsServer.HandlePublish = func(conn *joy4rtmp.Conn) {
			pullPL := func(url *url.URL) error {
				// pulls the playlist
				resp, err := http.Get(url.String())
				if err != nil {
					// log / complain
					return err
				}
				// record list of segments w/ properties
				/*
									type MediaPlaylist struct {
					    TargetDuration float64
					    SeqNo          uint64 // EXT-X-MEDIA-SEQUENCE
					    Segments       []*MediaSegment
					    Args           string // optional arguments placed after URIs (URI?Args)
					    Iframe         bool   // EXT-X-I-FRAMES-ONLY
					    Live           bool   // is this a VOD or Live (sliding window) playlist?
					    MediaType      MediaType

					    Key *Key // EXT-X-KEY is optional encryption key displayed before any segments (default key for the playlist)
					    Map *Map // EXT-X-MAP is optional tag specifies how to obtain the Media Initialization Section (default map for the playlist)
					    WV  *WV  // Widevine related tags outside of M3U8 specs
					    // contains filtered or unexported fields
					}
				*/
				//
				// deuplication + append
				playlist, listType, err := m3u8.DecodeFrom(resp.Body, false)
				if err != nil {
					// if we errror here, somebody is going to ask us to do it again anyway
					// log, complain
					return err
				}
				//switch v := listType.(type) {
				//if mpl, err := listType.(MediaPlaylist); err != nil {
				//
				//}
				var list *m3u8.MediaPlaylist
				if listType == m3u8.MASTER {
					master := playlist.(*m3u8.MasterPlaylist)
					if len(master.Variants) == 0 {
						return fmt.Errorf("no variants!")
					}
					list = master.Variants[0].Chunklist
				} else if listType == m3u8.MEDIA {
					list = playlist.(*m3u8.MediaPlaylist)
				}

				//case m3u8.MEDIA:
				//}
				return nil
			}
			// pull pl & later updates
			/// pull pl
			// plUrl
			ticker := time.NewTicker(1 * time.Second) // review this later, change to variable, peek into stream detect segment length yadda yadda
			//quit := make(chan struct{})
			go func() {
				for {
					select {
					case <-ticker.C:
						err := pullPL(plURL)
						if err != nil {
							// log it!
						}
					}
				}
			}()

		}

		////////
		self.RtmpServer.HandlePublish = func(conn *joy4rtmp.Conn) {
			glog.V(2).Infof("RTMP server got upstream: %v", conn.URL)

			strmID := makeStreamID(conn.URL)
			if strmID == "" {
				conn.Close()
				return
			}
			s := stream.NewBasicRTMPVideoStream(strmID)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			eof, err := s.WriteRTMPToStream(ctx, conn)
			if err != nil {
				return
			}

			err = gotStream(conn.URL, s)
			defer func() {
				endStream(conn.URL, s)
				conn.Close()
			}()
			if err != nil {
				glog.Errorf("Error RTMP gotStream handler: %v", err)
				return
			}

			select {
			case <-eof:
				// log something
			}
		}

	}
}
