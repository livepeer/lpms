[![Build Status](https://circleci.com/gh/livepeer/lpms.svg?style=shield&circle-token=e33534f6f4e2a6af19bb1596d7b72767a246cbab)](https://circleci.com/gh/livepeer/lpms/tree/master)

# LPMS - Livepeer media server

LPMS is a media server that can run independently, or on top of the [Livepeer](https://livepeer.org) 
network.  It allows you to manipulate / broadcast a live video stream.  Currently, LPMS supports RTMP
as input format and RTMP/HLS as output formats.

LPMS can be integrated into another service, or run as a standalone service.  To try LPMS as a 
standalone service, simply get the package:
```
go get -d github.com/livepeer/lpms/cmd/example
```

Go to the lpms root directory at `$GOPATH/src/github.com/livepeer/lpms`. If needed, install the required dependencies; see the Requirements section below. Then build the sample app and run it:

```
go build cmd/example/main.go
./example
```

### Requirements

LPMS requires libavcodec (ffmpeg) and friends. See `install_ffmpeg.sh` . Running this script will install everything in `~/compiled`. In order to build LPMS, the dependent libraries need to be discoverable by pkg-config and golang. If you installed everything with `install_ffmpeg.sh` , then run `export PKG_CONFIG_PATH=~/compiled/lib/pkgconfig:$PKG_CONFIG_PATH` so the deps are picked up.

Running golang unit tests (`test.sh`) requires the `ffmpeg` and `ffprobe` executables in addition to the libraries. However, none of these are run-time requirements; the executables are not used outside of testing, and the libraries are statically linked by default. Note that dynamic linking may substantially speed up rebuilds if doing heavy development.

### Testing out LPMS

The test LPMS server exposes a few different endpoints:
1. `rtmp://localhost:1935/stream/test` for uploading/viewing RTMP video stream.
2. `http://localhost:8000/stream/test_hls.m3u8` for consuming the HLS video stream.

Do the following steps to view a live stream video:
1. Start LPMS by running `go run cmd/example/main.go`

2. Upload an RTMP video stream to `rtmp://localhost:1935/stream/test`.  We recommend using ffmpeg or [OBS](https://obsproject.com/download).

For ffmpeg on osx, run: `ffmpeg -f avfoundation -framerate 30 -pixel_format uyvy422 -i "0:0" -vcodec libx264 -tune zerolatency -b 900k -x264-params keyint=60:min-keyint=60 -f flv rtmp://localhost:1935/stream/test`

For OBS, fill in Settings->Stream->URL to be rtmp://localhost:1935

3. If you have successfully uploaded the stream, you should see something like this in the LPMS output
```
I0324 09:44:14.639405   80673 listener.go:28] RTMP server got upstream
I0324 09:44:14.639429   80673 listener.go:42] Got RTMP Stream: test
```
4. Now you have a RTMP video stream running, we can view it from the server.  Simply run `ffplay http://localhost:8000/stream/test.m3u8`, you should see the hls video playback.


### Integrating LPMS

LPMS exposes a few different methods for customization. As an example, take a look at `cmd/lpms.go`.

To create a new LPMS server:
```
//Specify ports you want the server to run on, and which port SRS is running on (should be specified)
//in srs.conf
lpms := lpms.New("1936", "8000", "2436", "7936")
```

To handle RTMP publish:
```
	lpms.HandleRTMPPublish(
		//getStreamID
		func(reqPath string) (string, error) {
			return getStreamIDFromPath(reqPath), nil
		},
		//getStream
		func(reqPath string) (stream.Stream, stream.Stream, error) {
			rtmpStreamID := getStreamIDFromPath(reqPath)
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
```

To handle RTMP playback:
```
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
```

To handle HLS playback:
```
	lpms.HandleHLSPlay(
		//getHLSBuffer
		func(reqPath string) (*stream.HLSBuffer, error) {
			streamID := getHLSStreamIDFromPath(reqPath)
			buffer := bufferDB.db[streamID]
			s := streamDB.db[streamID]

			if s == nil {
				return nil, stream.ErrNotFound
			}

			if buffer == nil {
				//Create the buffer and start copying the stream into the buffer
				buffer = stream.NewHLSBuffer()
				bufferDB.db[streamID] = buffer

                //Subscribe to the stream
				sub := stream.NewStreamSubscriber(s)
				go sub.StartHLSWorker(context.Background())
				err := sub.SubscribeHLS(streamID, buffer)
				if err != nil {
					return nil, stream.ErrStreamSubscriber
				}
			}

			return buffer, nil
		})
```

You can follow the development of LPMS and Livepeer @ our [forum](http://forum.livepeer.org)
