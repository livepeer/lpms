#ifndef _LPMS_FFMPEG_H_
#define _LPMS_FFMPEG_H_

#include <libavutil/hwcontext.h>
#include <libavutil/rational.h>

// LPMS specific errors
extern const int lpms_ERR_INPUT_PIXFMT;
extern const int lpms_ERR_FILTERS;
extern const int lpms_ERR_OUTPUTS;

struct transcode_thread;

typedef struct {
    char *name;
    AVDictionary *opts;
} component_opts;

typedef struct {
  char *fname;
  char *vfilters;
  int w, h, bitrate;
  AVRational fps;

  component_opts muxer;
  component_opts audio;
  component_opts video;

} output_params;

typedef struct {
  char *fname;

  // Handle to a transcode thread.
  // If null, a new transcode thread is allocated.
  // The transcode thread is returned within `output_results`.
  // Must be freed with lpms_transcode_stop.
  struct transcode_thread *handle;

  // Optional hardware acceleration
  enum AVHWDeviceType hw_type;
  char *device;
} input_params;

typedef struct {
    int frames;
    int64_t pixels;

  // Returns
  int ret; // return code for this segment

} output_results;

void lpms_init();
int  lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int  lpms_transcode(input_params *inp, output_params *params, output_results *results, int nb_outputs, output_results *decoded_results);
struct transcode_thread* lpms_transcode_new();
void lpms_transcode_stop(struct transcode_thread* handle);

#endif // _LPMS_FFMPEG_H_
