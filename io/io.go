package io

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/swarm/storage/streaming"
	"github.com/golang/groupcache/lru"
	"github.com/kz26/m3u8"
	lpmsCommon "github.com/livepeer/lpms/common"
	"github.com/livepeer/lpms/types"
	"github.com/nareix/joy4/av"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
)

func CopyChannelToChannel(inChan chan *streaming.VideoChunk, outChan chan *streaming.VideoChunk) {
	for {
		select {
		case chunk := <-inChan:
			outChan <- chunk
		default:
		}
	}
}

func Transcode(inChan chan *streaming.VideoChunk, outChan chan *streaming.VideoChunk, newStreamID streaming.StreamID,
	format string, bitrate string, codecin string, codecout string, closeStreamC chan bool) (err error) {
	if codecin != "RTMP" {
		return fmt.Errorf("Only support RTMP as input stream")
	}

	if format != "HLS" {
		return fmt.Errorf("Only support HLS as output format")
	}

	if bitrate != "1000" && bitrate != "500" {
		return fmt.Errorf("Only support 500 and 1000 bitrate")
	}

	dstConn, err := joy4rtmp.Dial("rtmp://localhost:" + lpmsCommon.GetConfig().SrsRTMPPort + "/stream/" + string(newStreamID))
	if err != nil {
		glog.V(logger.Error).Infof("Error connecting to SRS server: ", err)
		return err
	}

	//Upload the video to SRS
	go CopyRTMPFromChannel(dstConn, inChan, closeStreamC)

	msChan := make(chan *types.Download, 1024)
	m3u8Chan := make(chan []byte)
	hlsSegChan := make(chan streaming.HlsSegment)
	//Download the playlist
	go GetHlsPlaylist("http://localhost:"+lpmsCommon.GetConfig().SrsHTTPPort+"/stream/"+string(newStreamID)+"_hls"+bitrate+".m3u8", time.Duration(0), true, msChan, m3u8Chan)
	//Download the segments
	go DownloadHlsSegment(msChan, hlsSegChan)
	//Copy the playlist and hls segments to a stream
	go CopyHlsToChannel(m3u8Chan, hlsSegChan, outChan, closeStreamC)

	return
}

//Copy packets from channels in the streamer to our destination muxer
func CopyRTMPFromStream(dst av.Muxer, stream *streaming.Stream, closeStreamC chan bool) (err error) {
	if len(stream.SrcVideoChan) > 0 {
		//First check SrcVideoChan, and then check DstVideoChan
		CopyRTMPFromChannel(dst, stream.SrcVideoChan, closeStreamC)
	} else {
		CopyRTMPFromChannel(dst, stream.DstVideoChan, closeStreamC)
	}

	return
}

func CopyRTMPFromChannel(dst av.Muxer, videoChan chan *streaming.VideoChunk, closeStreamC chan bool) (err error) {
	chunk := <-videoChan
	if err := dst.WriteHeader(chunk.HeaderStreams); err != nil {
		fmt.Println("Error writing header copying from channel")
		return err
	}

	for {
		select {
		case chunk := <-videoChan:
			// fmt.Println("Copying from channel")
			if chunk.ID == streaming.EOFStreamMsgID {
				fmt.Println("Copying EOF from channel")
				closeStreamC <- true
				err := dst.WriteTrailer()
				if err != nil {
					fmt.Println("Error writing trailer: ", err)
					return err
				}
			}
			err := dst.WritePacket(chunk.Packet)
			if chunk.Seq%100 == 0 {
				glog.V(logger.Info).Infof("Copy RTMP to muxer from channel. %d", chunk.Seq)
			}
			if err != nil {
				glog.V(logger.Error).Infof("Error writing packet to video player: %s", err)
				return err
			}
		}
	}
}

//Copy HLS segments and playlist to the streamer channel.
func CopyHlsToChannel(m3u8Chan chan []byte, hlsSegChan chan streaming.HlsSegment, outChan chan *streaming.VideoChunk, closeStreamC chan bool) {
	for {
		select {
		case m3u8 := <-m3u8Chan:
			// stream.M3U8 = m3u8 //Just for testing
			fmt.Printf("Sending HLS Playlist: %s\n", string(m3u8))
			CopyPacketsToChannel(1, nil, nil, m3u8, streaming.HlsSegment{}, outChan, closeStreamC)
		case hlsSeg := <-hlsSegChan:
			regex, _ := regexp.Compile("-(\\d)*")
			match := regex.FindString(hlsSeg.Name)
			segNumStr := match[1:len(match)]
			segNum, _ := strconv.Atoi(segNumStr)
			// stream.HlsSegNameMap[hlsSeg.Name] = hlsSeg.Data //Just for testing
			fmt.Printf("Sending HLS Segment: %d, %s\n", segNum, segNumStr)
			CopyPacketsToChannel(int64(segNum), nil, nil, nil, hlsSeg, outChan, closeStreamC)
		}
	}
}

// func CopyHlsToChannel(stream *streaming.Stream) (err error) {
// 	for {
// 		select {
// 		case m3u8 := <-stream.M3U8Chan:
// 			// stream.M3U8 = m3u8 //Just for testing
// 			fmt.Printf("Sending HLS Playlist: %s\n", string(m3u8))
// 			CopyPacketsToChannel(1, nil, nil, m3u8, streaming.HlsSegment{}, stream.SrcVideoChan)
// 		case hlsSeg := <-stream.HlsSegChan:
// 			regex, _ := regexp.Compile("-(\\d)*")
// 			match := regex.FindString(hlsSeg.Name)
// 			segNumStr := match[1:len(match)]
// 			segNum, _ := strconv.Atoi(segNumStr)
// 			// stream.HlsSegNameMap[hlsSeg.Name] = hlsSeg.Data //Just for testing
// 			fmt.Printf("Sending HLS Segment: %d, %s\n", segNum, segNumStr)
// 			CopyPacketsToChannel(int64(segNum), nil, nil, nil, hlsSeg, stream.SrcVideoChan)
// 		}
// 	}
// }

//Copy packets from our source demuxer to the streamer channels.  For now we put the header in every packet.  We can
//optimize for packet size later.
func CopyToChannel(src av.Demuxer, stream *streaming.Stream, closeStreamC chan bool) (err error) {
	var streams []av.CodecData
	if streams, err = src.Streams(); err != nil {
		return
	}
	for seq := int64(0); ; seq++ {
		if err = CopyPacketsToChannel(seq, src, streams, nil, streaming.HlsSegment{}, stream.SrcVideoChan, closeStreamC); err != nil {
			return
		}
	}
	return
}

// func CopyPacketsToChannel(seq int64, src av.PacketReader, headerStreams []av.CodecData, m3u8 []byte, hlsSeg streaming.HlsSegment, stream *streaming.Stream) (err error) {
func CopyPacketsToChannel(seq int64, src av.PacketReader, headerStreams []av.CodecData, m3u8 []byte, hlsSeg streaming.HlsSegment, outVideoChan chan *streaming.VideoChunk, closeStreamC chan bool) (err error) {
	// for seq := int64(0); ; seq++ {
	var pkt av.Packet
	if src != nil {
		if pkt, err = src.ReadPacket(); err != nil {
			if err == io.EOF {
				chunk := &streaming.VideoChunk{
					ID:            streaming.EOFStreamMsgID,
					Seq:           seq,
					HeaderStreams: headerStreams,
					Packet:        pkt,
				}
				// stream.SrcVideoChan <- chunk
				outVideoChan <- chunk
				fmt.Println("Done with packet reading: ", err)

				// Close the channel so that the protocol.go loop
				// reading from the channel doesn't block
				close(outVideoChan)
				closeStreamC <- true
				return fmt.Errorf("EOF")
			}
			return
		}
	}

	chunk := &streaming.VideoChunk{
		ID:            streaming.DeliverStreamMsgID,
		Seq:           seq,
		HeaderStreams: headerStreams,
		Packet:        pkt,
		M3U8:          m3u8,
		HLSSegData:    hlsSeg.Data,
		HLSSegName:    hlsSeg.Name,
	}

	select {
	// case stream.SrcVideoChan <- chunk:
	case outVideoChan <- chunk:
		if chunk.Seq%100 == 0 {
			fmt.Printf("sent video chunk: %d, %s\n", chunk.Seq, hlsSeg.Name)
		}
	default:
	}

	return
}

func doRequest(c *http.Client, req *http.Request) (*http.Response, error) {
	// req.Header.Set("User-Agent", USER_AGENT)
	resp, err := c.Do(req)
	return resp, err
}

func DownloadHlsSegment(dlc chan *types.Download, segChan chan streaming.HlsSegment) {
	for v := range dlc {
		req, err := http.NewRequest("GET", v.URI, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(&http.Client{}, req)
		if err != nil {
			log.Print(err)
			continue
		}
		if resp.StatusCode != 200 {
			log.Printf("Received HTTP %v for %v\n", resp.StatusCode, v.URI)
			continue
		}

		// Get the segment name - need to store in a map
		match := strings.Split(v.URI, "/")
		filename := match[len(match)-1]
		buf := new(bytes.Buffer)
		_, err = io.Copy(buf, resp.Body)
		if err != nil {
			log.Fatal(err)
		}

		seg := &streaming.HlsSegment{
			Data: buf.Bytes(),
			Name: filename,
		}
		// fmt.Println("Got HLS segment: ", filename)

		segChan <- *seg
		resp.Body.Close()
		// log.Printf("Downloaded %v\n", v.URI)
	}
}

func GetHlsPlaylist(urlStr string, recTime time.Duration, useLocalTime bool, dlc chan *types.Download, playlistChan chan []byte) {
	fmt.Println("Getting playlist: ", urlStr)
	startTime := time.Now()
	var recDuration time.Duration = 0
	cache := lru.New(1024)
	playlistUrl, err := url.Parse(urlStr)
	if err != nil {
		log.Fatal(err)
	}
	for {
		req, err := http.NewRequest("GET", urlStr, nil)
		if err != nil {
			log.Fatal(err)
		}
		resp, err := doRequest(&http.Client{}, req)
		if err != nil {
			log.Print(err)
			time.Sleep(time.Duration(3) * time.Second)
		}

		playlist, listType, err := m3u8.DecodeFrom(resp.Body, true)
		if playlist == nil {
			//SRS doesn't serve the video right away.  It take a few seconds.  May be a param we can tune later.
			waitTime := time.Second * 5
			fmt.Println("Cannot read playlist from ", urlStr, resp.StatusCode, "Waiting", waitTime)
			time.Sleep(waitTime)
		} else {
			// fmt.Println("Got playlist", urlStr)
			buf := playlist.Encode()
			bytes := buf.Bytes()
			// fmt.Println("sending playlist to playlistChan", bytes)
			playlistChan <- bytes
			resp.Body.Close()
			if listType == m3u8.MEDIA {
				mpl := playlist.(*m3u8.MediaPlaylist)
				for _, v := range mpl.Segments {
					if v != nil {
						var msURI string
						if strings.HasPrefix(v.URI, "http") {
							msURI, err = url.QueryUnescape(v.URI)
							if err != nil {
								log.Fatal(err)
							}
						} else {
							msUrl, err := playlistUrl.Parse(v.URI)
							if err != nil {
								log.Print(err)
								continue
							}
							msURI, err = url.QueryUnescape(msUrl.String())
							if err != nil {
								log.Fatal(err)
							}
						}
						_, hit := cache.Get(msURI)
						if !hit {
							cache.Add(msURI, nil)
							if useLocalTime {
								recDuration = time.Now().Sub(startTime)
							} else {
								recDuration += time.Duration(int64(v.Duration * 1000000000))
							}
							dlc <- &types.Download{
								URI:           msURI,
								TotalDuration: recDuration}
						}
						if recTime != 0 && recDuration != 0 && recDuration >= recTime {
							close(dlc)
							return
						}
					}
				}
				if mpl.Closed {
					close(dlc)
					return
				} else {
					time.Sleep(time.Duration(int64(mpl.TargetDuration * 1000000000)))
				}
			} else {
				log.Fatal("Not a valid media playlist")
			}
		}
	}
}

func rememberHlsSegs(nameSegMap *map[string][]byte, segChan chan streaming.HlsSegment) {
	for {
		select {
		case seg := <-segChan:
			fmt.Println("Got a HLS segment:", seg.Name)
			(*nameSegMap)[seg.Name] = seg.Data
		}
	}
}

func createTranscodeId(streamID streaming.StreamID, bReq types.BroadcastReq) common.Hash {
	//Create a "transcodeID" in the same keyspace as stream.ID
	fmt.Println("Creating transcode ID with: ", streamID, bReq)
	h := sha256.New()
	h.Write([]byte(streamID))
	h.Write([]byte(fmt.Sprintf("%v", bReq)))
	id := h.Sum(nil)

	var x common.Hash
	if len(x) != len(id) {
		panic("Error creating trasncode ID")
	}
	for i := 0; i < len(x); i++ {
		x[i] = id[i]
	}

	fmt.Println("Transcode ID: ", x)

	return x
}
