#ifndef _LPMS_ENCODER_H_
#define _LPMS_ENCODER_H_

#include "decoder.h"
#include "transcoder.h"
#include "filter.h"

int open_output(struct output_ctx *octx, struct input_ctx *ictx);
int reopen_output(struct output_ctx *octx, struct input_ctx *ictx);
int open_output1(struct output_ctx *octx, struct decode_meta *dmeta);
int reopen_output1(struct output_ctx *octx, struct decode_meta *dmeta);
void close_output(struct output_ctx *octx);
void free_output(struct output_ctx *octx);
int process_out(struct input_ctx *ictx, struct output_ctx *octx, AVCodecContext *encoder, AVStream *ost,
  struct filter_ctx *filter, AVFrame *inf);
int process_out1(struct decode_meta *dmeta, struct output_ctx *octx, AVCodecContext *encoder, AVStream *ost,
  struct filter_ctx *filter, AVFrame *inf);
int mux(AVPacket *pkt, AVRational tb, struct output_ctx *octx, AVStream *ost);

#endif // _LPMS_ENCODER_H_
