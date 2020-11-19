#include <stdio.h>

#include "/home/josh/compiled/include/libavformat/avformat.h"
#include "/home/josh/code/lpms/ffmpeg/lpms_ffmpeg.h"

int main(int argc, char **argv) {
  lpms_init();

  struct transcode_thread *t = lpms_transcode_new();
  int i = 0;
  for (i = 0; i < 4; i++) {
    char fname[64];
    char oname[64];
    snprintf(fname, sizeof fname, "in/bbb%d.ts", i);
    snprintf(oname, sizeof oname, "out/bbb%d.ts", i);
    AVDictionary *dict = NULL;
    av_dict_set(&dict, "forced-idr", "1", 0);
    input_params inp = {
      handle: t,
      fname: fname,
      hw_type: AV_HWDEVICE_TYPE_CUDA,
      //hw_type: AV_HWDEVICE_TYPE_NONE,
      device: "0"
    };
    output_params out[] = { {
      fname: oname,
      video: { name: "h264_nvenc", opts: dict },
      //video: { name: "libx264" },
      audio: { name: "aac" },
      vfilters: "fps=30/1,scale_cuda=w=640:h=480",
      //vfilters: "fps=30/1,scale_npp=w=640:h=480",
      //vfilters: "fps=30/1,scale=w=640:h=480",
      //vfilters: "hwdownload,format=nv12,fps=30/1,scale=w=640:h=480,hwupload_cuda=device=1",
      fps: {30, 1}
    } };
    output_results res[4] = {{0}, {0}, {0}, {0}};
    output_results decoded_res  = {0};
    fprintf(stderr, "Transcoding %s\n", fname);
    int ret = lpms_transcode(&inp, out, &res[0], 1, &decoded_res);
    av_dict_free(&out[0].video.opts);
    if (ret < 0) {
      fprintf(stderr, "Error transcoding\n");
      break;
    }
  }
  lpms_transcode_stop(t);
  return 0;
}
