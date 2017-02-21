//Adding the RTMP server.  This will put up a RTMP endpoint when starting up Swarm.
//It's a simple RTMP server that will take a video stream and play it right back out.
//After bringing up the Swarm node with RTMP enabled, try it out using:
//
//ffmpeg -re -i bunny.mp4 -c copy -f flv rtmp://localhost/movie
//ffplay rtmp://localhost/movie

package lpms

import (
	"encoding/json"

	"fmt"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bytes"

	"github.com/ethereum/go-ethereum/logger"
	"github.com/ethereum/go-ethereum/logger/glog"
	"github.com/ethereum/go-ethereum/swarm/storage"
	"github.com/ethereum/go-ethereum/swarm/storage/streaming"
	"github.com/golang/groupcache/lru"
	"github.com/kz26/m3u8"

	streamingVizClient "github.com/livepeer/streamingviz/client"
	"github.com/nareix/joy4/av"
	"github.com/nareix/joy4/av/avutil"
	"github.com/nareix/joy4/format"
	"github.com/nareix/joy4/format/flv"
	joy4rtmp "github.com/nareix/joy4/format/rtmp"
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

func init() {
	format.RegisterAll()
}

//For now, the SRS server listens to RTMP on a different port, and publishes transcoded HLS video over http.
func StartSRSServer(srsRtmpPort string, srsHttpPort string) {
	//Start the SRS Server
	glog.V(logger.Info).Infof("Starting SRS server on http:%s, rtmp:%s", srsHttpPort, srsRtmpPort)

	cmd := exec.Command("./bin/srs", "-c", "srs.conf")
	cmd.Start()
	cmd.Wait()
}

//Spin off a go routine that serves rtmp requests.  For now I think this only handles a single stream.
//Has to take a http port because we want to support viewing over HLS, flv over http, etc.
//I recognize the srs stuff is extra.  We can get rid of it when we switch to using ffmpeg.
func StartVideoServer(rtmpPort string, httpPort string, srsRtmpPort string, srsHttpPort string, streamer *streaming.Streamer, forwarder storage.CloudStore, viz *streamingVizClient.Client) {
	if rtmpPort == "" {
		rtmpPort = "1935"
	}
	fmt.Println("Starting RTMP Server on port: ", rtmpPort)
	server := &joy4rtmp.Server{Addr: ":" + rtmpPort}

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
		stream, err := streamer.SubscribeToStream(strmID)

		if err != nil {
			glog.V(logger.Info).Infof("Error subscribing to stream %v", err)
			return
		}

		//Send subscribe request
		forwarder.Stream(strmID)

		//Copy chunks to outgoing connection
		go CopyFromChannel(conn, stream)
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
			msChan := make(chan *Download, 1024)

			//Copy to SRS rtmp
			go avutil.CopyFile(dstConn, conn)
			//Kick off goroutine to listen for HLS playlist file
			go getHlsPlaylist("http://localhost:"+srsHttpPort+"/stream/"+string(stream.ID)+".m3u8", time.Duration(0), true, msChan, stream.M3U8Chan)
			//Download the segments
			go downloadHlsSegment(msChan, stream.HlsSegChan)
			//Copy Hls segments to swarm
			go CopyHlsToChannel(stream)
		} else {
			//Do regular RTMP stuff - create a new stream, copy the video to the stream.
			stream, _ := streamer.AddNewStream()
			glog.V(logger.Info).Infof("Added a new stream with id: %v", stream.ID)
			viz.LogBroadcast(string(stream.ID))

			//Send video to streamer channels
			go CopyToChannel(conn, stream)
		}
	}

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
				forwarder.Stream(strmID)
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
				// keys := make([]string, 0, len(stream.HlsSegNameMap))
				// for k := range stream.HlsSegNameMap {
				//  keys = append(keys, k)
				// }
				// fmt.Println("Available segments: ", keys)

				if stream.HlsSegNameMap[filename] != nil {
					fmt.Println("Writing requested segment: ", filename)
					// w.Header().Set("Content-Type", "video/MP2T")
					w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(r.URL.Path)))
					// w.Header().Set("Content-Length", string(len(stream.HlsSegNameMap[filename])))
					w.Write(stream.HlsSegNameMap[filename])
					// w.WriteHeader(http.StatusOK)
					//Should probably remove the seg at some point.  For now let's just keep it around
					//in case another client requests
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
				forwarder.Stream(strmID)
			}

			w.Header().Set("Content-Type", "video/x-flv")
			w.Header().Set("Transfer-Encoding", "chunked")
			w.WriteHeader(200)
			flusher := w.(http.Flusher)
			flusher.Flush()

			muxer := flv.NewMuxerWriteFlusher(writeFlusher{httpflusher: flusher, Writer: w})
			//Cannot kick off a go routine here because the ResponseWriter is not a pointer (so a copy of the writer doesn't make any sense)
			CopyFromChannel(muxer, stream)
		}
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
	// go startSRS(srsRtmpPort, srsHttpPort)
	server.ListenAndServe()
}

type Ports struct {
	RtmpPort string
	HttpPort string
}

func startSRS(srsRtmpPort string, srsHttpPort string) {
	ports := Ports{srsRtmpPort, srsHttpPort}
	tmpl, err := template.ParseFiles("srs.tmpl")
	if err != nil {
		glog.V(logger.Error).Infof("Cannot load srs.tmpl, cannot start srs")
		return
	}
	f, err := os.OpenFile("srs.conf", os.O_CREATE|os.O_WRONLY, 0777)
	if err != nil {
		glog.V(logger.Error).Infof("Cannot create srs.conf, cannot start srs")
		return
	}
	err = tmpl.ExecuteTemplate(f, "srs.tmpl", ports)
	if err != nil {
		glog.V(logger.Error).Infof("Cannot write srs.conf, cannot start srs")
		fmt.Println(err)
		return
	}

	fmt.Println("Starting srs...")
	cmd := exec.Command("./bin/srs", "-c", "srs.conf")
	cmd.Start()
	cmd.Wait()
}

//Copy packets from channels in the streamer to our destination muxer
func CopyFromChannel(dst av.Muxer, stream *streaming.Stream) (err error) {
	chunk := <-stream.DstVideoChan
	if err = dst.WriteHeader(chunk.HeaderStreams); err != nil {
		fmt.Println("Error writing header copying from channel")
		return
	}

	for {
		select {
		case chunk := <-stream.DstVideoChan:
			// fmt.Println("Copying from channel")
			if chunk.ID == streaming.EOFStreamMsgID {
				fmt.Println("Copying EOF from channel")
				err := dst.WriteTrailer()
				if err != nil {
					fmt.Println("Error writing trailer: ", err)
					return err
				}
			}
			err := dst.WritePacket(chunk.Packet)
			if err != nil {
				glog.V(logger.Error).Infof("Error writing packet to video player: %s", err)
				return err
			}
			// This doesn't work because default will just end the stream too quickly.
			// There is a design trade-off here: if we want the stream to automatically continue after some kind of
			// interruption, then we cannot end the stream.  Maybe we can do it after like... 10 mins of inactivity,
			// but it's quite common for livestream sources to have some difficulties and stop for minutes at a time.
			// default:
			//  fmt.Println("CopyFromChannel Finished")
			//  return
		}
	}
}

//Copy HLS segments and playlist to the streamer channel.
func CopyHlsToChannel(stream *streaming.Stream) (err error) {
	for {
		select {
		case m3u8 := <-stream.M3U8Chan:
			// stream.M3U8 = m3u8 //Just for testing
			CopyPacketsToChannel(0, nil, nil, m3u8, streaming.HlsSegment{}, stream)
		case hlsSeg := <-stream.HlsSegChan:
			regex, _ := regexp.Compile("-(\\d)*")
			match := regex.FindString(hlsSeg.Name)
			segNumStr := match[1:len(match)]
			segNum, _ := strconv.Atoi(segNumStr)
			// stream.HlsSegNameMap[hlsSeg.Name] = hlsSeg.Data //Just for testing
			CopyPacketsToChannel(int64(segNum), nil, nil, nil, hlsSeg, stream)
		}
	}
}

//Copy packets from our source demuxer to the streamer channels.  For now we put the header in every packet.  We can
//optimize for packet size later.
func CopyToChannel(src av.Demuxer, stream *streaming.Stream) (err error) {
	var streams []av.CodecData
	if streams, err = src.Streams(); err != nil {
		return
	}
	for seq := int64(0); ; seq++ {
		if err = CopyPacketsToChannel(seq, src, streams, nil, streaming.HlsSegment{}, stream); err != nil {
			return
		}
	}
	return
}

func CopyPacketsToChannel(seq int64, src av.PacketReader, headerStreams []av.CodecData, m3u8 []byte, hlsSeg streaming.HlsSegment, stream *streaming.Stream) (err error) {
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
				stream.SrcVideoChan <- chunk
				fmt.Println("Done with packet reading: ", err)

				// Close the channel so that the protocol.go loop
				// reading from the channel doesn't block
				close(stream.SrcVideoChan)
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
	case stream.SrcVideoChan <- chunk:
		if chunk.Seq%100 == 0 {
			fmt.Printf("sent video chunk: %d, %s\n", chunk.Seq, chunk.HLSSegName)
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

type Segment struct {
	Data []byte
	Name string
}

type Download struct {
	URI           string
	totalDuration time.Duration
}

func downloadHlsSegment(dlc chan *Download, segChan chan streaming.HlsSegment) {
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
		fmt.Println("Got HLS segment: ", filename)

		segChan <- *seg
		resp.Body.Close()
		// log.Printf("Downloaded %v\n", v.URI)
	}
}

func getHlsPlaylist(urlStr string, recTime time.Duration, useLocalTime bool, dlc chan *Download, playlistChan chan []byte) {
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
							dlc <- &Download{msURI, recDuration}
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
