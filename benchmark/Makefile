GOSRC=/home/josh/code/lpms/ffmpeg
LIBS=libavcodec libavfilter libavformat libavutil

all: gpu

test: test.c
	gcc -Wall `pkg-config --cflags $(LIBS) ` $(GOSRC)/lpms_ffmpeg.c -g test.c `pkg-config --libs $(LIBS)`

test_conc: test_conc.c
	#$(CC) -fsanitize=memory `pkg-config --cflags $(LIBS) ` $(GOSRC)/lpms_ffmpeg.c -g test_conc.c `pkg-config --libs $(LIBS)` -lpthread
	$(CC) `pkg-config --cflags $(LIBS) ` $(GOSRC)/lpms_ffmpeg.c -g test_conc.c `pkg-config --libs $(LIBS)` -lpthread

test_multi: test_multi.c
	$(CC) -Wall -fsanitize=address `pkg-config --cflags $(LIBS)` $(GOSRC)/lpms_ffmpeg.c -g test_multi.c -fsanitize=address `pkg-config --libs $(LIBS)` -lpthread
	$(CC) -Wall `pkg-config --cflags $(LIBS)` $(GOSRC)/lpms_ffmpeg.c -g test_multi.c `pkg-config --libs $(LIBS)` -lpthread

test_rand: test_rand.c
	$(CC) -Wall `pkg-config --cflags $(LIBS)` $(GOSRC)/lpms_ffmpeg.c -g $^ `pkg-config --libs $(LIBS)` -lpthread

memory: memory.c
	gcc `pkg-config --cflags $(LIBS) ` $(GOSRC)/lpms_ffmpeg.c -g memory.c `pkg-config --libs $(LIBS)`

.PHONY: gpu
gpu: gpu.go
	CPATH=/usr/local/cuda/include LIBRARY_PATH=/usr/local/cuda/lib64 go build $^

bench:
	go build lpms-bench.go

buffered:
	go build buffered-bench.go

nonp-buffered:
	go build nonp-buffered-bench.go

rand:
	go build rand-bench.go

.PHONY: hls
hls:
	go build hls.go

clean:
	rm -f gpu lpms-bench
