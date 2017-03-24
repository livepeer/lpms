# LPMS - Livepeer media server

LPMS is a media server that can run independently, or on top of the [Livepeer](https://livepeer.org) 
network.  It allows you to manipulate / broadcast a live video stream.  Currently, LPMS supports RTMP
as input format and RTMP/HLS as output formats.

LPMS can be integrated into another service, or run as a standalone service.  To try LPMS as a 
standalone service, simply get the package:
```
go get github.com/livepeer/lpms
```

Go to the lpms root directory, and run 
```
./lpms
```

### Requirements

LPMS requires ffmpeg.  To install it on OSX, use homebrew.  As a part of this installation, `ffmpeg` and `ffplay` should be installed as commandline utilities.

```
//This may take a few minutes
brew install ffmpeg --with-sdl2 --with-libx264
```

LPMS uses [SRS](http://ossrs.net/srs.release/releases/) as a transcoding backend.  It's included in 
the `/bin` directory for testing purposes. Make sure you are running SRS before testing out LPMS.

To start srs, run 
```
./bin/srs -c ./bin/srs.conf
```

### Testing out LPMS

The test LPMS server exposes a few different endpoints:
1. `rtmp://localhost:1936/stream/test` for uploading/viewing RTMP video stream.
2. `http://localhost:8000/transcode` for issuing transcode request.
3. `http://localhost:8000/stream/test_tran.m3u8` for consuming the transcoded video.

Do the following steps to view a live stream video:
1. Upload an RTMP video stream to `rtmp://localhost:1936/stream/test`.  We recommend using [OBS](https://obsproject.com/download).


![OBS Screenshot](https://s3.amazonaws.com/livepeer/obs_screenshot.png)


2. If you have successfully uploaded the stream, you should see something like this in the LPMS output
```
I0324 09:44:14.639405   80673 listener.go:28] RTMP server got upstream
I0324 09:44:14.639429   80673 listener.go:42] Got RTMP Stream: test
```
3. Now you have a RTMP video stream running, we can view it from the server.  Simply run `ffplay rtmp://localhost:1936/stream/test`, you should see the rtmp video playback.
4. Let's transcode the video to HLS.  Before issuing the transcoding request, make sure your SRS is running.
```
//To start SRS
./bin/srs -c ./bin/srs.conf
```

![SRS Screenshot](https://s3.amazonaws.com/livepeer/srs_screenshot.png)

5. To issue the transcoding request, we can use curl.
```
curl -H "Content-Type: application/json" -X POST -d '{"StreamID":"test"}' http://localhost:8000/transcode
```

5. You should see your SRS console start logging.  Now just open up `hlsVideo.html` in Safari, and you should see the HLS video.  There may be a delay due to the video transcoding - we'll expose more parameters to lower that delay in the future.  Note that in typical internet broadcasting today, there is usually a delay of 30 - 90 seconds.


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
    func(reqPath string) (*stream.Stream, error) {
        streamID := getStreamIDFromPath(reqPath)
        stream := stream.NewStream(streamID)
        streamDB.db[streamID] = stream
        return stream, nil
    },
    //finishStream
    func(reqPath string) {
        delete(streamDB.db, getStreamIDFromPath(reqPath))
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

To handle transcode request:
```
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
```

To handle HLS playback:
```
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
        }
        return buffer, nil

    })
```

You can follow the development of LPMS and Livepeer @ our [forum](http://forum.livepeer.org)
