//Adding the RTMP server.  This will put up a RTMP endpoint when starting up Swarm.
//It's a simple RTMP server that will take a video stream and play it right back out.
//After bringing up the Swarm node with RTMP enabled, try it out using:
//
//ffmpeg -re -i bunny.mp4 -c copy -f flv rtmp://localhost/movie
//ffplay rtmp://localhost/movie

package lpms

import (
	"github.com/ethereum/go-ethereum/swarm/network"
	"github.com/ethereum/go-ethereum/swarm/storage"
	"github.com/ethereum/go-ethereum/swarm/storage/streaming"
	"github.com/livepeer/lpms/common"
	"github.com/livepeer/lpms/server"
	streamingVizClient "github.com/livepeer/streamingviz/client"
)

func StartVideoServer(rtmpPort string, httpPort string, srsRtmpPort string, srsHttpPort string, streamer *streaming.Streamer,
	forwarder storage.CloudStore, streamdb *network.StreamDB, viz *streamingVizClient.Client) {

	common.SetConfig(srsRtmpPort, srsHttpPort, rtmpPort, httpPort)
	server.StartRTMPServer(rtmpPort, srsRtmpPort, srsHttpPort, streamer, forwarder, viz)
	server.StartHTTPServer(rtmpPort, httpPort, srsRtmpPort, srsHttpPort, streamer, forwarder, streamdb, viz)

}
