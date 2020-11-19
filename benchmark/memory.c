#include <stdio.h>

#include "/home/josh/compiled/include/libavformat/avformat.h"
#include "/home/josh/code/lpms/ffmpeg/lpms_ffmpeg.h"

int main(int argc, char **argv) {
  lpms_init();

  struct transcode_thread *t = lpms_transcode_new();
  input_params inp = {
    .handle = t,
    .fname = "bbb.mp4",
  };
  output_params out[] = { {
    .fname = "out/c_bbb.ts",
    .video = {.name = "libx264"},
    .audio = {.name = "copy"},
    .vfilters = "fps=30/1,scale=w=640:h=480",
    .fps = {30, 1}
  } };
  output_results res = {0}, decoded = {0};
  lpms_transcode(&inp, out, &res, 1, &decoded);
  lpms_transcode_stop(t);
  return 0;
}
