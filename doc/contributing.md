# Contributing
Thank you for your interest in contributing to Livepeer Media Server.

# Working on Go code
If your goal is making changes solely to Go part of the codebase, just follow [Requirements](https://github.com/livepeer/lpms/#requirements) section and make sure you have all dependencies to build Go scripts successfully. Debugging with Delve, GDB, or, visually, with IDE (e.g. JetBrains GoLand, Microsoft VS Code) should work fine.

# Working on CGO interface and FFMpeg code
When working on CGO interface, or [FFmpeg](https://github.com/livepeer/FFmpeg/) fork, chances are you might want to create a separate debug build of all components. 

To do it:
1. Copy `~/compiled` dir created by `install_ffmpeg.sh` to `compiled_debug`
2. If you want to keep system-wide Ffmpeg installation, remove/rename system `pkg-config` (.pc) files of Ffmpeg libs (located in `/usr/local/lib/pkgconfig/` on most Linux distros). Otherwise, `pkg-config` will prioritize system libs without debug information.
3. [FFmpeg](https://github.com/livepeer/FFmpeg/) should be already cloned after running `install_ffmpeg.sh`. Navigate to FFmpeg dir and run `configure` as below, making sure all paths are correct.
    ```
    export ROOT=/projects/livepeer/src
    export PATH="$ROOT/compiled_debug/bin":$PATH
    export PKG_CONFIG_PATH="${PKG_CONFIG_PATH:-}:$ROOT/compiled_debug/lib/pkgconfig"
    ./configure --fatal-warnings \
        --enable-gnutls --enable-libx264 --enable-gpl \
        --enable-protocol=https,http,rtmp,file,pipe \
        --enable-muxer=mpegts,hls,segment,mp4,null --enable-demuxer=flv,mpegts,mp4,mov \
        --enable-bsf=h264_mp4toannexb,aac_adtstoasc,h264_metadata,h264_redundant_pps,extract_extradata \
        --enable-parser=aac,aac_latm,h264 \
        --enable-filter=abuffer,buffer,abuffersink,buffersink,afifo,fifo,aformat,format \
        --enable-filter=aresample,asetnsamples,fps,scale,hwdownload,select,livepeer_dnn,signature \
        --enable-encoder=aac,libx264 \
        --enable-decoder=aac,h264 \
        --extra-cflags="-I${ROOT}/compiled_debug/include -g3 -ggdb -fno-inline" \
        --extra-ldflags="-L${ROOT}/compiled_debug/lib ${EXTRA_LDFLAGS}" \
        --enable-cuda --enable-cuda-llvm --enable-cuvid --enable-nvenc --enable-decoder=h264_cuvid --enable-filter=scale_cuda --enable-encoder=h264_nvenc \
        --enable-libtensorflow \
        --prefix="$ROOT/compiled_debug" \
        --enable-debug=3                \
        --disable-optimizations \
        --disable-stripping
    ```
4. Run `make install`
5. Make sure there are debug symbols in newly built static libs:
    ```
    objdump --syms compiled_debug/lib/libavcodec.a
    ```
6. Now you can build Go apps with debug information. Use absolute path in `PKG_CONFIG_PATH` to point to `compiled_debug` dir:
    ```
    PKG_CONFIG_PATH=/projects/livepeer/src/compiled_debug/lib/pkgconfig go build -a -gcflags all="-N -l" cmd/scenedetection/scenedetection.go
    ```
7. Check that Go executable linked correctly. You shouldn't see any FFmpeg's dynamic libraries in LDD output for newly built binary (e.g. `libavcodec.so`).
    ```
    ldd ./scenedetection
    ```
7. Finally, run the binary with GDB. If linking with dynamic libraries, `LD_LIBRARY_PATH` should point to `compiled_debug` dir:
    ```
    LD_LIBRARY_PATH="../compiled_debug/lib/" gdb --args scenedetection 0 ../bbb/source.m3u P144p30fps16x9 nv 0
    ```
8. Optionally, you can install `gdbgui` with `pip install gdbgui` and use it in place of `gdb` for visual debugging.  