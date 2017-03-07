package server

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/swarm/network/kademlia"
	"github.com/livepeer/go-livepeer/livepeer/storage"
	"github.com/livepeer/go-livepeer/livepeer/storage/streaming"
	"github.com/livepeer/lpms/io"
	"github.com/livepeer/lpms/types"
	streamingVizClient "github.com/livepeer/streamingviz/client"
	"github.com/nareix/joy4/av/avutil"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

var srsRTMPPort string

func SrsRTMPPort() string {
	return srsRTMPPort
}

func StartRTMPServer(rtmpPort string, srsRtmpPort string, srsHttpPort string, streamer *streaming.Streamer, forwarder storage.CloudStore, viz *streamingVizClient.Client) {
	if rtmpPort == "" {
		rtmpPort = "1935"
	}
	fmt.Println("Starting RTMP Server on port: ", rtmpPort)
	server := &joy4rtmp.Server{Addr: ":" + rtmpPort}

	srsRTMPPort = srsRtmpPort

	server.HandlePlay = func(conn *joy4rtmp.Conn) {
		glog.V(logger.Info).Infof("Trying to play stream at %v", conn.URL)

		// Parse the streamID from the path host:port/stream/{streamID}
		var strmID string
		regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
		match := regex.FindString(conn.URL.Path)
		if match != "" {
			strmID = strings.Replace(match, "/stream/", "", -1)
		}

		glog.V(logger.Info).Infof("Got streamID as %v", strmID)
		viz.LogConsume(strmID)
		stream, err := streamer.GetStreamByStreamID(streaming.StreamID(strmID))
		if stream == nil {
			stream, err = streamer.SubscribeToStream(strmID)
			if err != nil {
				glog.V(logger.Info).Infof("Error subscribing to stream %v", err)
				return
			}
		} else {
			fmt.Println("Found stream: ", strmID)
		}

		//Send subscribe request
		forwarder.Stream(strmID, kademlia.Address(common.HexToHash("")))

		//Copy chunks to outgoing connection
		go io.CopyRTMPFromStream(conn, stream, stream.CloseChan)
	}

	server.HandlePublish = func(conn *joy4rtmp.Conn) {
		transcodeParam := conn.URL.Query()["transcode"]
		if (len(transcodeParam) > 0) && (transcodeParam[0] == "true") {
			//For now, we rely on SRS. The next iteraion will be looking into directly integrating ffmpeg
			//First, forward the rtmp stream to the local SRS server (always running on .
			//Then, issue http req through the HLS endpoint.
			stream, _ := streamer.AddNewStream()
			glog.V(logger.Info).Infof("Added a new stream with id: %v", stream.ID)
			viz.LogBroadcast(string(stream.ID))
			dstConn, err := joy4rtmp.Dial("rtmp://localhost:" + srsRtmpPort + "/stream/" + string(stream.ID))
			if err != nil {
				glog.V(logger.Error).Infof("Error connecting to SRS server: ", err)
				return
			}

			//To pass segment name from the playlist to the segment download routine.
			msChan := make(chan *types.Download, 1024)

			//Copy to SRS rtmp
			go avutil.CopyFile(dstConn, conn)
			//Kick off goroutine to listen for HLS playlist file
			go io.GetHlsPlaylist("http://localhost:"+srsHttpPort+"/stream/"+string(stream.ID)+".m3u8", time.Duration(0), true, msChan, stream.M3U8Chan)
			//Download the segments
			go io.DownloadHlsSegment(msChan, stream.HlsSegChan)
			//Copy Hls segments to swarm
			go io.CopyHlsToChannel(stream.M3U8Chan, stream.HlsSegChan, stream.SrcVideoChan, stream.CloseChan)
			// go io.CopyHlsToChannel(stream)
		} else {
			//Do regular RTMP stuff - create a new stream, copy the video to the stream.
			var strmID string
			var stream *streaming.Stream
			regex, _ := regexp.Compile("\\/stream\\/([[:alpha:]]|\\d)*")
			match := regex.FindString(conn.URL.Path)
			if match != "" {
				strmID = strings.Replace(match, "/stream/", "", -1)
				stream, _ = streamer.GetStreamByStreamID(streaming.StreamID(strmID))
			}

			if stream == nil {
				stream, _ = streamer.AddNewStream()
				glog.V(logger.Info).Infof("Added a new stream with id: %v", stream.ID)
			} else {
				glog.V(logger.Info).Infof("Got streamID as %v", strmID)
			}

			viz.LogBroadcast(string(stream.ID))

			//Send video to streamer channels
			go io.CopyToChannel(conn, stream, stream.CloseChan)
		}
	}

	go server.ListenAndServe()
}
