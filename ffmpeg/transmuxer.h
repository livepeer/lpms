#ifndef _LPMS_TRANSMUXER_H_
#define _LPMS_TRANSMUXER_H_

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/hwcontext.h>
#include <libavutil/rational.h>

#include "transcoder.h"

typedef struct {
  char *fname;

  component_opts muxer;
} m_output_params;

typedef struct {
  char *fname;

  // Handle to a transcode thread.
  // If null, a new transcode thread is allocated.
  // The transcode thread is returned within `output_results`.
  // Must be freed with lpms_transcode_stop.
  struct transmuxe_thread *handle;

} m_input_params;

int lpms_transmuxe(m_input_params *inp, m_output_params *params,
                   output_results *results);
struct transmuxe_thread *lpms_transmuxe_new();
void lpms_transmuxe_stop(struct transmuxe_thread *handle);
void lpms_transmuxe_discontinuity(struct transmuxe_thread *handle);

#endif // _LPMS_TRANSMUXER_H_
