#ifndef _LPMS_FFMPEG_H_
#define _LPMS_FFMPEG_H_

#include <libavutil/rational.h>

typedef struct {
  char *fname;
  int w, h, bitrate;
  AVRational fps;
} output_params;

void lpms_init();
int  lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int  lpms_transcode(char *inp, output_params *params, int nb_outputs);

#endif // _LPMS_FFMPEG_H_
