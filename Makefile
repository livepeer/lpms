main: ffmpeg/lpms_ffmpeg.c cmd/example/main.go
	go build cmd/example/main.go

stream:
	~/compiled/bin/ffmpeg -f lavfi -i anullsrc=channel_layout=stereo:sample_rate=44100 -re -i ~/bbb_sunflower_1080p_30fps_normal.mp4 -vf scale=640:360 -vcodec libx264 -b:v 600k -x264-params keyint=24:min-keyint=24:scenecut=0:bframes=2:b-adapt=0:ref=2:b-adapt=0:open-gop=1 -acodec aac -ac 1 -b:a 96k -f flv rtmp://127.0.0.1:1935/stream/test

clean:
	rm -rf main hls_out

.PHONY: clean stream
