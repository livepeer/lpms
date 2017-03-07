package server

import (
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/livepeer/go-livepeer/livepeer/network"
	"github.com/ethereum/go-ethereum/swarm/network/kademlia"
	"github.com/livepeer/go-livepeer/livepeer/storage"
	"github.com/livepeer/go-livepeer/livepeer/storage/streaming"
	lpmsIo "github.com/livepeer/lpms/io"
	streamingVizClient "github.com/livepeer/streamingviz/client"
	"github.com/nareix/joy4/format/flv"
)

//This is for flushing to http request handlers (joy4 concept)
type writeFlusher struct {
	httpflusher http.Flusher
	io.Writer
}

func (self writeFlusher) Flush() error {
	self.httpflusher.Flush()
	return nil
}

type broadcastReq struct {
	Formats  []string
	Bitrates []string
	Codecin  string
	Codecout []string
	StreamID string
}

func StartHTTPServer(rtmpPort string, httpPort string, srsRtmpPort string, srsHttpPort string, streamer *streaming.Streamer, forwarder storage.CloudStore, streamdb *network.StreamDB, viz *streamingVizClient.Client) {
	glog.V(logger.Info).Infof("Starting HTTP Server at port: ", httpPort)

	http.HandleFunc("/stream/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("In handleFunc, Path: ", r.URL.Path)

		var strmID string
		//Example path: /stream/133bd3c4e543e3cd53e2cf2b366eeeace7eae483b651b8b1e2a2072b250864fc62b0bac9f64df186c4fb74d427f136647dcf0ead9198dc7d9f881b1d5c2d2132-0.ts
		regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
		match := regex.FindString(r.URL.Path)
		if match != "" {
			strmID = strings.Replace(match, "/stream/", "", -1)
		}

		glog.V(logger.Info).Infof("Got streamID as %v", strmID)

		if strings.HasSuffix(r.URL.Path, ".m3u8") == true {
			stream, err := streamer.GetStreamByStreamID(streaming.StreamID(strmID))
			if stream == nil {
				stream, err = streamer.SubscribeToStream(strmID)
				if err != nil {
					glog.V(logger.Info).Infof("Error subscribing to stream %v", err)
					return
				}
				//Send subscribe request
				forwarder.Stream(strmID, kademlia.Address(common.HexToHash("")))
			}

			//HLS request. Example: http://localhost:8080/stream/streamid.m3u8
			countdown := 12
			for countdown > 0 {
				if stream.M3U8 != nil {
					break
				} else {
					fmt.Println("Waiting for playlist")
					time.Sleep(time.Second * 5)
				}
				countdown = countdown - 1
			}
			if countdown == 0 {
				w.WriteHeader(404)
				w.Write([]byte("Cannot find playlist for HLS"))
			}
			// w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Write(stream.M3U8)
			fmt.Println("Writing Playlist in handler: ", string(stream.M3U8))
			// go rememberHlsSegs(&stream.HlsSegNameMap, stream.HlsSegChan) // this is only used for testing viewer on publisher.  Publisher doesn't need to remember HLS segments
			// return
		} else if strings.HasSuffix(r.URL.Path, ".ts") == true {
			//HLS video segments

			stream, _ := streamer.GetStreamByStreamID(streaming.StreamID(strmID))
			fmt.Println("Got requests for: ", r.URL.Path)
			match := strings.Split(r.URL.Path, "/")
			filename := match[len(match)-1]

			countdown := 60 //Total wait time is 60 seconds.  Make the single wait smaller to minimize total delay.
			for countdown > 0 {
				if stream.HlsSegNameMap[filename] != nil {
					w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
					w.Write(stream.HlsSegNameMap[filename])
					break
				} else {
					fmt.Println("Waiting 1s for segment", filename, ", ", countdown)
					time.Sleep(time.Second * 1)
				}
				countdown = countdown - 1
			}

			if countdown == 0 {
				w.WriteHeader(500)
				return
			}
		} else {
			//Assume rtmp
			fmt.Println("Assumign rtmp: ", r.URL.Path)
			stream, err := streamer.GetStreamByStreamID(streaming.StreamID(strmID))
			if stream == nil {
				stream, err = streamer.SubscribeToStream(strmID)
				if err != nil {
					glog.V(logger.Info).Infof("Error subscribing to stream %v", err)
					return
				}
				//Send subscribe request
				forwarder.Stream(strmID, kademlia.Address(common.HexToHash("")))
			}

			w.Header().Set("Content-Type", "video/x-flv")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(200)
			flusher := w.(http.Flusher)
			flusher.Flush()

			muxer := flv.NewMuxerWriteFlusher(writeFlusher{httpflusher: flusher, Writer: w})
			//Cannot kick off a go routine here because the ResponseWriter is not a pointer (so a copy of the writer doesn't make any sense)
			lpmsIo.CopyRTMPFromStream(muxer, stream, stream.CloseChan)
		}
	})

	http.HandleFunc("/broadcast", func(w http.ResponseWriter, r *http.Request) {
		glog.V(logger.Info).Infof("Got broadcast request")
		decoder := json.NewDecoder(r.Body)
		var bReq broadcastReq
		if r.Body == nil {
			http.Error(w, "Please send a request body", 400)
			return
		}
		err := decoder.Decode(&bReq)
		// glog.V(logger.Info).Infof("http body: ", r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		bReq.Codecin = "RTMP"
		glog.V(logger.Info).Infof("Broadcast request: ", bReq)

		var transcodeId common.Hash
		if len(r.URL.Query()["transcodeId"]) > 0 {
			str := r.URL.Query()["transcodeId"][0]
			transcodeId = common.HexToHash(str)
			glog.V(logger.Info).Infof("transcodeId %x", transcodeId[:])
		} else {
			//generate an completely random id
			transcodeId = common.HexToHash(string(streaming.MakeStreamID(streaming.RandomStreamID(), fmt.Sprintf("%x", streaming.RandomStreamID()))))
		}

		// streamID := r.URL.Query()["streamId"][0]
		streamID := bReq.StreamID
		stream, _ := streamer.GetStreamByStreamID(streaming.StreamID(streamID))
		if stream == nil {
			// stream, _ = streamer.AddNewStream()
			//Require a stream to exist first
			w.WriteHeader(404)
			w.Write([]byte("Cannot find stream with ID: " + streamID))
		}
		forwarder.Transcode(string(stream.ID), transcodeId, bReq.Formats, bReq.Bitrates, bReq.Codecin, bReq.Codecout)
		glog.V(logger.Info).Infof("Broadcast Original Stream: %s.  Waiting for ack...", stream.ID)
	})

	http.HandleFunc("/transcodedVideo", func(w http.ResponseWriter, r *http.Request) {
		glog.V(logger.Info).Infof("Getting transcoded video")
		videos := streamdb.TranscodedStreams[streaming.StreamID(r.URL.Query()["originStreamID"][0])]
		js, err := json.Marshal(videos)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})

	http.HandleFunc("/streamIDs", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Getting stream ids")
		streams := streamer.GetAllStreams()
		js, err := json.Marshal(streams)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
		return
	})

	http.HandleFunc("/streamEndpoint", func(w http.ResponseWriter, r *http.Request) {
		fmt.Println("Getting stream endpoint")
		resp := map[string]string{"url": "rtmp://localhost:" + rtmpPort + "/live/stream"}
		js, _ := json.Marshal(resp)

		w.Header().Set("Content-Type", "application/json")
		w.Write(js)
	})

	//For serving static HTML files (web-based broadcaster and viewer)
	fs := http.FileServer(http.Dir("static"))
	fmt.Println("Serving static files from: ", fs)
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/static/broadcast.html", 301)
	})

	go http.ListenAndServe(":"+httpPort, nil)
}
