//The RTMP server.  This will put up a RTMP endpoint when starting up Swarm.
//To integrate with LPMS means your code will become the source / destination of the media server.
//This RTMP endpoint is mainly used for video upload.  The expected url is rtmp://localhost:port/livepeer/stream
package lpms

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/golang/glog"
	"github.com/livepeer/lpms/stream"
	"github.com/livepeer/lpms/transcoder"
	"github.com/livepeer/lpms/vidlistener"
	"github.com/livepeer/lpms/vidplayer"
	"github.com/nareix/joy4/av"

	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

type LPMS struct {
	rtmpServer  *joy4rtmp.Server
	vidPlayer   *vidplayer.VidPlayer
	vidListen   *vidlistener.VidListener
	httpPort    string
	srsRTMPPort string
	srsHTTPPort string
}

type transcodeReq struct {
	Formats  []string
	Bitrates []string
	Codecin  string
	Codecout []string
	StreamID string
}

//New creates a new LPMS server object.  It really just brokers everything to the components.
func New(rtmpPort string, httpPort string, srsRTMPPort string, srsHTTPPort string) *LPMS {
	server := &joy4rtmp.Server{Addr: (":" + rtmpPort)}
	player := &vidplayer.VidPlayer{RtmpServer: server}
	listener := &vidlistener.VidListener{RtmpServer: server}
	return &LPMS{rtmpServer: server, vidPlayer: player, vidListen: listener, srsRTMPPort: srsRTMPPort, srsHTTPPort: srsHTTPPort, httpPort: httpPort}
}

//Start starts the rtmp and http server
func (l *LPMS) Start() error {
	ec := make(chan error, 1)
	go func() {
		glog.Infof("Starting LPMS Server at :%v", l.rtmpServer.Addr)
		ec <- l.rtmpServer.ListenAndServe()
	}()
	go func() {
		glog.Infof("Starting HTTP Server at :%v", l.httpPort)
		ec <- http.ListenAndServe(":"+l.httpPort, nil)
	}()

	select {
	case err := <-ec:
		glog.Infof("LPMS Server Error: %v.  Quitting...", err)
		return err
	}
}

//HandleRTMPPublish offload to the video listener
func (l *LPMS) HandleRTMPPublish(
	getStreamID func(reqPath string) (string, error),
	stream func(reqPath string) (*stream.Stream, error),
	endStream func(reqPath string)) error {

	return l.vidListen.HandleRTMPPublish(getStreamID, stream, endStream)
}

//HandleRTMPPlay offload to the video player
func (l *LPMS) HandleRTMPPlay(getStream func(ctx context.Context, reqPath string, dst av.MuxCloser) error) error {
	return l.vidPlayer.HandleRTMPPlay(getStream)
}

//HandleHLSPlay offload to the video player
func (l *LPMS) HandleHLSPlay(getStream func(reqPath string) (*stream.HLSBuffer, error)) error {
	return l.vidPlayer.HandleHLSPlay(getStream)
}

//HandleTranscode kicks off a transcoding process, keeps a local HLS buffer, and returns the new stream ID.
//stream is the video stream you want to be transcoded.  getNewStreamID gives you a way to name the transcoded stream.
func (l *LPMS) HandleTranscode(getInStream func(ctx context.Context, streamID string) (*stream.Stream, error), getOutStream func(ctx context.Context, streamID string) (*stream.Stream, error)) {
	http.HandleFunc("/transcode", func(w http.ResponseWriter, r *http.Request) {
		ctx, _ := context.WithCancel(context.Background())
		// defer cancel()

		//parse transcode request
		decoder := json.NewDecoder(r.Body)
		var tReq transcodeReq
		if r.Body == nil {
			http.Error(w, "Please send a request body", 400)
			return
		}
		err := decoder.Decode(&tReq)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		//Get the RTMP Stream
		inStream, err := getInStream(ctx, tReq.StreamID)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		//Get the HLS Stream
		newStream, err := getOutStream(ctx, tReq.StreamID)
		if err != nil {
			http.Error(w, err.Error(), 400)
		}

		ec := make(chan error, 1)
		go func() { ec <- l.doTranscoding(ctx, inStream, newStream) }()

		w.Write([]byte("New Stream: " + newStream.StreamID))
	})
}

func (l *LPMS) doTranscoding(ctx context.Context, inStream *stream.Stream, newStream *stream.Stream) error {
	t := transcoder.New(l.srsRTMPPort, l.srsHTTPPort, newStream.StreamID)
	//Should kick off a goroutine for this, so we can return the new streamID rightaway.

	tranMux, err := t.LocalSRSUploadMux()
	if err != nil {
		return err
		// http.Error(w, "Cannot create a connection with local transcoder", 400)
	}

	uec := make(chan error, 1)
	go func() { uec <- t.StartUpload(ctx, tranMux, inStream) }()
	dec := make(chan error, 1)
	go func() { dec <- t.StartDownload(ctx, newStream) }()

	select {
	case err := <-uec:
		return err
		// http.Error(w, "Cannot upload stream to transcoder: "+err.Error(), 400)
	case err := <-dec:
		return err
		// http.Error(w, "Cannot download stream from transcoder: "+err.Error(), 400)
	}

}
