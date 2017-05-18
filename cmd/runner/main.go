package main

import (
	"context"
	"flag"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang/glog"
	"github.com/livepeer/lpms"
	"github.com/livepeer/lpms/stream"

	"github.com/nareix/joy4/av"
)

type StreamDB struct {
	db map[string]stream.Stream
}

type BufferDB struct {
	db map[string]*stream.HLSBuffer
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	lpms := lpms.New("1935", "8000", "", "")
	streamDB := &StreamDB{db: make(map[string]stream.Stream)}
	bufferDB := &BufferDB{db: make(map[string]*stream.HLSBuffer)}

	lpms.HandleRTMPPublish(
		//getStreamID
		func(url *url.URL) (string, error) {
			return getStreamIDFromPath(url.Path), nil
		},
		//getStream
		func(url *url.URL) (stream.Stream, stream.Stream, error) {
			rtmpStreamID := getStreamIDFromPath(url.Path)
			hlsStreamID := rtmpStreamID + "_hls"
			rtmpStream := stream.NewVideoStream(rtmpStreamID, stream.RTMP)
			hlsStream := stream.NewVideoStream(hlsStreamID, stream.HLS)
			streamDB.db[rtmpStreamID] = rtmpStream
			streamDB.db[hlsStreamID] = hlsStream
			return rtmpStream, hlsStream, nil
		},
		//finishStream
		func(rtmpID string, hlsID string) {
			delete(streamDB.db, rtmpID)
			delete(streamDB.db, hlsID)
		})

	//No transcoding for now until segment transcoder is finished.
	// lpms.HandleTranscode(
	// 	//getInStream
	// 	func(ctx context.Context, streamID string) (stream.Stream, error) {
	// 		if stream := streamDB.db[streamID]; stream != nil {
	// 			return stream, nil
	// 		}

	// 		return nil, stream.ErrNotFound
	// 	},
	// 	//getOutStream
	// 	func(ctx context.Context, streamID string) (stream.Stream, error) {
	// 		//For this example, we'll name the transcoded stream "{streamID}_tran"
	// 		newStream := stream.NewVideoStream(streamID + "_tran")
	// 		streamDB.db[newStream.GetStreamID()] = newStream
	// 		return newStream, nil

	// 		// glog.Infof("Making File Stream")
	// 		// fileStream := stream.NewFileStream(streamID + "_file")
	// 		// return fileStream, nil
	// 	})

	lpms.HandleHLSPlay(
		//getHLSBuffer
		func(reqPath string) (*stream.HLSBuffer, error) {
			streamID := getHLSStreamIDFromPath(reqPath)
			// glog.Infof("Got HTTP Req for stream: %v", streamID)
			buffer := bufferDB.db[streamID]
			s := streamDB.db[streamID]

			if s == nil {
				return nil, stream.ErrNotFound
			}

			if buffer == nil {
				//Create the buffer and start copying the stream into the buffer
				buffer = stream.NewHLSBuffer(10, 100)
				bufferDB.db[streamID] = buffer
				sub := stream.NewStreamSubscriber(s)
				go sub.StartHLSWorker(context.Background(), time.Second*1)
				err := sub.SubscribeHLS(streamID, buffer)
				if err != nil {
					return nil, stream.ErrStreamSubscriber
				}
			}

			return buffer, nil
		})

	lpms.HandleRTMPPlay(
		//getStream
		func(ctx context.Context, reqPath string, dst av.MuxCloser) error {
			glog.Infof("Got req: ", reqPath)
			streamID := getStreamIDFromPath(reqPath)
			src := streamDB.db[streamID]

			if src != nil {
				src.ReadRTMPFromStream(ctx, dst)
			} else {
				glog.Error("Cannot find stream for ", streamID)
				return stream.ErrNotFound
			}
			return nil
		})

	//Helper function to print out all the streams
	http.HandleFunc("/streams", func(w http.ResponseWriter, r *http.Request) {
		streams := []string{}

		for k, _ := range streamDB.db {
			streams = append(streams, k)
		}

		if len(streams) == 0 {
			w.Write([]byte("no streams"))
			return
		}
		str := strings.Join(streams, ",")
		w.Write([]byte(str))
	})

	lpms.Start()
}

func getStreamIDFromPath(reqPath string) string {
	return "test"
}

func getHLSStreamIDFromPath(reqPath string) string {
	return "test_hls"
}
