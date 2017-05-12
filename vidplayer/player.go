package vidplayer

import (
	"context"
	"mime"
	"net/http"
	"path"

	"strings"

	"time"

	"github.com/ericxtang/m3u8"
	"github.com/golang/glog"
	"github.com/livepeer/lpms/stream"
	"github.com/nareix/joy4/av"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

//VidPlayer is the module that handles playing video. For now we only support RTMP and HLS play.
type VidPlayer struct {
	RtmpServer *joy4rtmp.Server
}

//HandleRTMPPlay is the handler when there is a RTMP request for a video. The source should write
//into the MuxCloser. The easiest way is through avutil.Copy.
func (s *VidPlayer) HandleRTMPPlay(getStream func(ctx context.Context, reqPath string, dst av.MuxCloser) error) error {
	s.RtmpServer.HandlePlay = func(conn *joy4rtmp.Conn) {
		glog.Infof("LPMS got RTMP request @ %v", conn.URL)

		ctx := context.Background()
		c := make(chan error, 1)
		go func() { c <- getStream(ctx, conn.URL.Path, conn) }()
		select {
		case err := <-c:
			glog.Errorf("Rtmp getStream Error: %v", err)
			return
		}
	}
	return nil
}

//HandleHLSPlay is the handler when there is a HLA request. The source should write the raw bytes into the io.Writer,
//for either the playlist or the segment.
func (s *VidPlayer) HandleHLSPlay(getHLSBuffer func(reqPath string) (*stream.HLSBuffer, error)) error {
	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		handleHLS(w, r, getHLSBuffer)
	})
	return nil
}

func handleHLS(w http.ResponseWriter, r *http.Request, getHLSBuffer func(reqPath string) (*stream.HLSBuffer, error)) {
	glog.Infof("LPMS got HTTP request @ %v", r.URL.Path)

	if !strings.HasSuffix(r.URL.Path, ".m3u8") && !strings.HasSuffix(r.URL.Path, ".ts") {
		http.Error(w, "LPMS only accepts HLS requests over HTTP (m3u8, ts).", 500)
	}

	ctx := context.Background()
	buffer, err := getHLSBuffer(r.URL.Path)
	if err != nil {
		glog.Errorf("Error getting HLS Buffer: %v", err)
		return
	}

	if strings.HasSuffix(r.URL.Path, ".m3u8") {
		var pl *m3u8.MediaPlaylist
		for i := 0; i < 4; i++ {
			pl, err = buffer.LatestPlaylist()
			if pl.Count() == 0 {
				time.Sleep(500 * time.Millisecond)
			} else {
				break
			}
		}
		if err != nil {
			glog.Errorf("Error getting HLS playlist %v: %v", r.URL.Path, err)
			return
		}

		// //Remove all but the last 5 segments.
		// c := uint(0)
		// for _, seg := range pl.Segments {
		// 	if seg != nil {
		// 		// segs = append(segs, seg)
		// 		c = c + 1
		// 	}
		// }
		// for c > buffer.Capacity {
		// 	pl.Remove()
		// 	c = c - 1
		// }
		// pl.TargetDuration = 2

		// segs := ""
		// for _, s := range pl.Segments {
		// 	segs = segs + ", " + strings.Split(s.URI, "_")[1]
		// }
		// glog.Infof("Writing playlist seg: %v", segs)

		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
		w.Header().Set("Access-Control-Allow-Origin", "*")

		_, err = w.Write(pl.Encode().Bytes())
		if err != nil {
			glog.Errorf("Error writing playlist to ResponseWriter: %v", err)
			return
		}

		if err != nil {
			glog.Errorf("Error writting HLS playlist %v: %v", r.URL.Path, err)
			return
		}
		return
	}

	if strings.HasSuffix(r.URL.Path, ".ts") {
		pathArr := strings.Split(r.URL.Path, "/")
		segName := pathArr[len(pathArr)-1]
		// seg, err := buffer.WaitAndPopSegment(ctx, segName)
		seg, err := buffer.WaitAndGetSegment(ctx, segName)
		if err != nil {
			glog.Errorf("Error getting HLS segment %v: %v", segName, err)
			return
		}
		// glog.Infof("Writing seg: %v, len:%v", segName, len(seg))
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
		w.Header().Set("Access-Control-Allow-Origin", "*")
		_, err = w.Write(seg)
		if err != nil {
			glog.Errorf("Error writting HLS segment %v: %v", segName, err)
			return
		}
		return
	}

	http.Error(w, "Cannot find HTTP video resource: "+r.URL.Path, 500)
}
