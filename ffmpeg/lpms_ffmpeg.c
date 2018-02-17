#include "lpms_ffmpeg.h"

#include <libavformat/avformat.h>

//
// Internal transcoder data structures
//
struct input_ctx {
  AVFormatContext *ic; // demuxer required
};

struct output_ctx {
  char *fname;         // required output file name
  int width, height, bitrate; // w, h, br required
  AVRational fps;
  AVFormatContext *oc; // muxer required
};

void lpms_init()
{
  avformat_network_init();
  av_log_set_level(AV_LOG_WARNING);
}

void lpms_deinit()
{
  avformat_network_deinit();
}

//
// Segmenter
//

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char* seg_time)
{
#define r2h_err(str) {\
  if (!ret) ret = 1; \
  errstr = str; \
  goto handle_r2h_err; \
}
  char *errstr          = NULL;
  int ret               = 0;
  AVFormatContext *ic   = NULL;
  AVFormatContext *oc   = NULL;
  AVOutputFormat *ofmt  = NULL;
  AVStream *ist         = NULL;
  AVStream *ost         = NULL;
  AVDictionary *md      = NULL;
  AVCodec *codec        = NULL;
  int64_t prev_ts[2]    = {AV_NOPTS_VALUE, AV_NOPTS_VALUE};
  int stream_map[2]     = {-1, -1};
  AVPacket pkt;

  ret = avformat_open_input(&ic, listen, NULL, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to open input\n");
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to find any input streams\n");

  ofmt = av_guess_format(NULL, outf, NULL);
  if (!ofmt) r2h_err("Could not deduce output format from file extension\n");
  ret = avformat_alloc_output_context2(&oc, ofmt, NULL, outf);
  if (ret < 0) r2h_err("Unable to allocate output context\n");

  stream_map[0] = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (stream_map[0] < 0) r2h_err("segmenter: Unable to find video stream\n");
  stream_map[1] = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
  if (stream_map[1] < 0) r2h_err("segmenter: Unable to find audio stream\n");

  ist = ic->streams[stream_map[0]];
  ost = avformat_new_stream(oc, NULL);
  if (!ost) r2h_err("segmenter: Unable to allocate output video stream\n");
  avcodec_parameters_copy(ost->codecpar, ist->codecpar);
  ist = ic->streams[stream_map[1]];
  ost = avformat_new_stream(oc, NULL);
  if (!ost) r2h_err("segmenter: Unable to allocate output audio stream\n");
  avcodec_parameters_copy(ost->codecpar, ist->codecpar);

  av_dict_set(&md, "hls_time", seg_time, 0);
  av_dict_set(&md, "hls_segment_filename", ts_tmpl, 0);
  ret = avformat_write_header(oc, &md);
  if (ret < 0) r2h_err("Error writing header\n");

  av_init_packet(&pkt);
  while (1) {
    ret = av_read_frame(ic, &pkt);
    if (ret == AVERROR_EOF) {
      av_interleaved_write_frame(oc, NULL); // flush
      break;
    } else if (ret < 0) r2h_err("Error reading\n");
    // rescale timestamps
    if (pkt.stream_index == stream_map[0]) pkt.stream_index = 0;
    else if (pkt.stream_index == stream_map[1]) pkt.stream_index = 1;
    else goto r2hloop_end;
    ist = ic->streams[pkt.stream_index];
    ost = oc->streams[stream_map[pkt.stream_index]];
    int64_t dts_next = pkt.dts, dts_prev = prev_ts[pkt.stream_index];
    if (AV_NOPTS_VALUE == dts_prev) dts_prev = dts_next;
    pkt.pts = av_rescale_q_rnd(pkt.pts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    pkt.dts = av_rescale_q_rnd(pkt.dts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    if (!pkt.duration) pkt.duration = dts_next - dts_prev;
    pkt.duration = av_rescale_q(pkt.duration, ist->time_base, ost->time_base);
    prev_ts[pkt.stream_index] = dts_next;
    // write the thing
    ret = av_interleaved_write_frame(oc, &pkt);
    if (ret < 0) r2h_err("segmenter: Unable to write output frame\n");
r2hloop_end:
    av_packet_unref(&pkt);
  }
  ret = av_write_trailer(oc);
  if (ret < 0) r2h_err("segmenter: Unable to write trailer\n");

handle_r2h_err:
  if (errstr) fprintf(stderr, "%s", errstr);
  if (ic) avformat_close_input(&ic);
  if (oc) avformat_free_context(oc);
  if (md) av_dict_free(&md);
  return ret == AVERROR_EOF ? 0 : ret;
}

//
// Transcoder
//

static void free_output(struct output_ctx *octx)
{
  if (octx->oc) {
    if (!(octx->oc->oformat->flags & AVFMT_NOFILE) && octx->oc->pb) {
      avio_closep(&octx->oc->pb);
    }
    avformat_free_context(octx->oc);
  }
}


static int open_output(struct output_ctx *octx, struct input_ctx *ictx)
{
#define em_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_output_err; \
}
  int ret = 0;
  AVOutputFormat *fmt = NULL;
  AVFormatContext *oc = NULL;
  AVStream *st        = NULL;

  // open muxer
  fmt = av_guess_format(NULL, octx->fname, NULL);
  if (!fmt) em_err("Unable to guess output format\n");
  ret = avformat_alloc_output_context2(&oc, fmt, NULL, octx->fname);
  if (ret < 0) em_err("Unable to alloc output context\n");
  octx->oc = oc;

  if (!(fmt->flags & AVFMT_NOFILE)) {
    avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0) em_err("Error opening output file\n");
  }

  ret = avformat_write_header(oc, NULL);
  if (ret < 0) em_err("Error writing header\n");

  return 0;

open_output_err:
  free_output(octx);
  return ret;
}

static void free_input(struct input_ctx *inctx)
{
  if (inctx->ic) avformat_close_input(&inctx->ic);
}

static int open_input(char *inp, struct input_ctx *ctx)
{
#define dd_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto open_input_err; \
}
  AVFormatContext *ic   = NULL;

  // open demuxer
  int ret = avformat_open_input(&ic, inp, NULL, NULL);
  if (ret < 0) dd_err("demuxer: Unable to open input\n");
  ctx->ic = ic;
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) dd_err("Unable to find input info\n");

  return 0;

open_input_err:
  free_input(ctx);
  return ret;
#undef dd_err
}

int process_in(struct input_ctx *ictx, AVPacket *pkt)
{
#define dec_err(msg) { \
  if (!ret) ret = -1; \
  fprintf(stderr, msg); \
  goto dec_cleanup; \
}
  int ret = 0;

  av_init_packet(pkt);
  // loop until a new packet has been read, or EAGAIN
  while (1) {
    ret = av_read_frame(ictx->ic, pkt);
    if (ret == AVERROR_EOF) return ret;
    else if (ret < 0) dec_err("Unable to read input\n");
  }

dec_cleanup:
  if (ret < 0) av_packet_unref(pkt); // XXX necessary? or have caller do it?
  return ret;

#undef dec_err
}

int process_out(struct output_ctx *octx, AVPacket *pkt)
{
#define proc_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) { fprintf(stderr, "u done messed up\n"); ret = AVERROR(ENOMEM); } \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s", msg, errstr); \
  goto proc_cleanup; \
}
  int ret = 0;

  ret = av_interleaved_write_frame(octx->oc, pkt);
  if (ret < 0) proc_err("Error writing frame\n"); // XXX handle better?

proc_cleanup:
  return ret;
#undef proc_err
}

#define MAX_OUTPUT_SIZE 10

int lpms_transcode(char *inp, output_params *params, int nb_outputs)
{
#define main_err(msg) { \
  if (!ret) ret = AVERROR(EINVAL); \
  fprintf(stderr, msg); \
  goto transcode_cleanup; \
}
  int ret = 0, i = 0;
  struct input_ctx ictx;
  AVPacket ipkt;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];
  AVFrame *dframe;

  memset(&ictx, 0, sizeof ictx);
  memset(outputs, 0, sizeof outputs);

  if (nb_outputs > MAX_OUTPUT_SIZE) main_err("transcoder: Too many outputs\n");

  // populate input context
  ret = open_input(inp, &ictx);
  if (ret < 0) main_err("transcoder: Unable to open input\n");

  // populate output contexts
  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    octx->fname = params[i].fname;
    octx->width = params[i].w;
    octx->height = params[i].h;
    if (params[i].bitrate) octx->bitrate = params[i].bitrate;
    if (params[i].fps.den) octx->fps = params[i].fps;
    ret = open_output(octx, &ictx);
    if (ret < 0) main_err("transcoder: Unable to open output\n");
  }

  av_init_packet(&ipkt);

  while (1) {
    AVStream *ist = NULL;
    ret = process_in(&ictx, &ipkt);
    if (ret == AVERROR_EOF) break;
    else if (ret < 0) goto whileloop_end; // XXX fix
    ist = ictx.ic->streams[ipkt.stream_index];

    for (i = 0; i < nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      AVStream *ost = NULL;
      ist = ictx.ic->streams[ipkt.stream_index];

      ret = process_out(octx, &ipkt);
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) main_err("transcoder: verybad\n");
    }

whileloop_end:
    av_packet_unref(&ipkt);
  }

  // flush outputs
  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    // only issue w this flushing method is it's not necessarily sequential
    // wrt all the outputs; might want to iterate on each output per frame?
    ret = 0;
    av_interleaved_write_frame(octx->oc, NULL); // flush muxer
    ret = av_write_trailer(octx->oc);
    if (ret < 0) main_err("transcoder: Unable to write trailer");
  }

transcode_cleanup:
  free_input(&ictx);
  for (i = 0; i < MAX_OUTPUT_SIZE; i++) free_output(&outputs[i]);
  return ret == AVERROR_EOF ? 0 : ret;
#undef main_err
}
