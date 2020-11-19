#include <stdio.h>

#include "/home/josh/compiled/include/libavformat/avformat.h"
#include "/home/josh/code/lpms/ffmpeg/lpms_ffmpeg.h"

int main(int argc, char **argv) {

  if (argc < 2) {
    fprintf(stderr, "Usage: %s <input-file>\n", argv[0]);
    return 1;
  }

  lpms_init();
  //av_log_set_level(AV_LOG_TRACE);

  struct transcode_thread *t = lpms_transcode_new();
  input_params inp = {
    handle: t,
    //fname: "bbb.mp4",
    //fname: "in/short_testharness.m3u8",
    fname: argv[1],
    hw_type: AV_HWDEVICE_TYPE_CUDA
  };
  output_params out[] = { {
    fname: "out/c_bbb.ts",
    video: { name: "h264_nvenc" },
    vfilters: "fps=30/1,scale_cuda=w=640:h=480",
    //video: { name: "libx264" },
    //vfilters: "fps=30/1,scale=w=640:h=480",
    //audio: { name: "copy" },
    //vfilters: "hwupload_cuda=0,scale_cuda=w=640:h=480",
    //vencoder: "libx264",
    //w: 1920,
    //h: 1080,
    fps: {30, 1}
  } };
  output_results res[4] = {{0},{0},{0},{0}};
  output_results decoded_res = {0};
  lpms_transcode(&inp, out, &res[0], 1, &decoded_res);
  lpms_transcode_stop(t);
  return 0;
}
