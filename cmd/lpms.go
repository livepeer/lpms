package main

import (
	"context"
	"flag"
	"net/http"
	"strings"

	"github.com/golang/glog"
	"github.com/livepeer/lpms"
	"github.com/livepeer/lpms/stream"

	"github.com/nareix/joy4/av"
)

type StreamDB struct {
	db map[string]*stream.Stream
}

type BufferDB struct {
	db map[string]*stream.HLSBuffer
}

func main() {
	flag.Set("logtostderr", "true")
	flag.Parse()

	lpms := lpms.New("1935", "8000", "2435", "7935")
	streamDB := &StreamDB{db: make(map[string]*stream.Stream)}
	bufferDB := &BufferDB{db: make(map[string]*stream.HLSBuffer)}

	lpms.HandleRTMPPublish(
		//getStreamID
		func(reqPath string) (string, error) {
			return getStreamIDFromPath(reqPath), nil
		},
		//getStream
		func(reqPath string) (*stream.Stream, error) {
			streamID := getStreamIDFromPath(reqPath)
			stream := stream.NewStream(streamID)
			streamDB.db[streamID] = stream
			return stream, nil
		},
		//finishStream
		func(reqPath string) {
			streamID := getStreamIDFromPath(reqPath)
			delete(streamDB.db, streamID)
			tranStreamID := streamID + "_tran"
			delete(streamDB.db, tranStreamID)
		})

	lpms.HandleTranscode(
		//getInStream
		func(ctx context.Context, streamID string) (*stream.Stream, error) {
			if stream := streamDB.db[streamID]; stream != nil {
				return stream, nil
			}

			return nil, stream.ErrNotFound
		},
		//getOutStream
		func(ctx context.Context, streamID string) (*stream.Stream, error) {
			//For this example, we'll name the transcoded stream "{streamID}_tran"
			newStream := stream.NewStream(streamID + "_tran")
			streamDB.db[newStream.StreamID] = newStream
			return newStream, nil
		})

	lpms.HandleHLSPlay(
		//getHLSBuffer
		func(reqPath string) (*stream.HLSBuffer, error) {
			streamID := getHLSStreamIDFromPath(reqPath)
			glog.Infof("Got HTTP Req for stream: %v", streamID)
			buffer := bufferDB.db[streamID]
			s := streamDB.db[streamID]

			if s == nil {
				return nil, stream.ErrNotFound
			}

			if buffer == nil {
				//Create the buffer and start copying the stream into the buffer
				buffer = stream.NewHLSBuffer()
				bufferDB.db[streamID] = buffer
				ec := make(chan error, 1)
				go func() { ec <- s.ReadHLSFromStream(buffer) }()
				//May want to handle the error here
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
	if strings.HasSuffix(reqPath, ".m3u8") {
		return "test_tran"
	} else {
		return "test_tran"
	}
}
