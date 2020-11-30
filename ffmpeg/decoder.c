#include "transcoder.h"
#include "decoder.h"
#include "logging.h"

#include <libavutil/pixfmt.h>

static int lpms_send_packet(struct input_ctx *ictx, AVCodecContext *dec, AVPacket *pkt)
{
    int ret = avcodec_send_packet(dec, pkt);
    if (ret == 0 && dec == ictx->vc) ictx->pkt_diff++; // increase buffer count for video packets
    return ret;
}

static int lpms_receive_frame(struct input_ctx *ictx, AVCodecContext *dec, AVFrame *frame)
{
    int ret = avcodec_receive_frame(dec, frame);
    if (dec != ictx->vc) return ret;
    if (!ret && frame && !is_flush_frame(frame)) {
      ictx->pkt_diff--; // decrease buffer count for non-sentinel video frames
      if (ictx->flushing) ictx->sentinel_count = 0;
    }
    return ret;
}

static void send_first_pkt(struct input_ctx *ictx)
{
  if (ictx->flushed || !ictx->first_pkt) return;

  int ret = avcodec_send_packet(ictx->vc, ictx->first_pkt);
  if (ret < 0) {
    LPMS_ERR(packet_cleanup, "Error sending flush packet");
  } else ictx->sentinel_count++;
packet_cleanup:
  return;
}

int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt)
{
  int ret = 0;

  // Read a packet and attempt to decode it.
  // If decoding was not possible, return the packet anyway for streamcopy
  av_init_packet(pkt);
  while (1) {
    AVStream *ist = NULL;
    AVCodecContext *decoder = NULL;
    ret = av_read_frame(ictx->ic, pkt);
    if (ret == AVERROR_EOF) goto dec_flush;
    else if (ret < 0) LPMS_ERR(dec_cleanup, "Unable to read input");
    ist = ictx->ic->streams[pkt->stream_index];
    if (ist->index == ictx->vi && ictx->vc) decoder = ictx->vc;
    else if (ist->index == ictx->ai && ictx->ac) decoder = ictx->ac;
    else if (pkt->stream_index == ictx->vi || pkt->stream_index == ictx->ai) break;
    else goto drop_packet; // could be an extra stream; skip

    if (!ictx->first_pkt && pkt->flags & AV_PKT_FLAG_KEY && decoder == ictx->vc) {
      ictx->first_pkt = av_packet_clone(pkt);
      ictx->first_pkt->pts = -1;
    }

    ret = lpms_send_packet(ictx, decoder, pkt);
    if (ret < 0) LPMS_ERR(dec_cleanup, "Error sending packet to decoder");
    ret = lpms_receive_frame(ictx, decoder, frame);
    if (ret == AVERROR(EAGAIN)) {
      // Distinguish from EAGAIN that may occur with
      // av_read_frame or avcodec_send_packet
      ret = lpms_ERR_PACKET_ONLY;
      break;
    }
    else if (ret < 0) LPMS_ERR(dec_cleanup, "Error receiving frame from decoder");
    break;

drop_packet:
    av_packet_unref(pkt);
  }

dec_cleanup:
  return ret;

dec_flush:

  // Attempt to read all frames that are remaining within the decoder, starting
  // with video. If there's a nonzero response type, we know there are no more
  // video frames, so continue on to audio.

  // Flush video decoder.
  // To accommodate CUDA, we feed the decoder sentinel (flush) frames, till we
  // get back all sent frames, or we've made SENTINEL_MAX attempts to retrieve
  // buffered frames with no success.
  // TODO this is unnecessary for SW decoding! SW process should match audio
  if (ictx->vc) {
    ictx->flushing = 1;
    send_first_pkt(ictx);
    ret = lpms_receive_frame(ictx, ictx->vc, frame);
    pkt->stream_index = ictx->vi;
    // Keep flushing if we haven't received all frames back but stop after SENTINEL_MAX tries.
    if (ictx->pkt_diff != 0 && ictx->sentinel_count <= SENTINEL_MAX && (!ret || ret == AVERROR(EAGAIN))) {
        return 0; // ignore actual return value and keep flushing
    } else {
        ictx->flushed = 1;
        if (!ret) return ret;
    }
  }
  // Flush audio decoder.
  if (ictx->ac) {
    avcodec_send_packet(ictx->ac, NULL);
    ret = avcodec_receive_frame(ictx->ac, frame);
    pkt->stream_index = ictx->ai;
    if (!ret) return ret;
  }
  return AVERROR_EOF;
}

// FIXME: name me and the other function better
enum AVPixelFormat hw2pixfmt(AVCodecContext *ctx)
{
  const AVCodec *decoder = ctx->codec;
  struct input_ctx *params = (struct input_ctx*)ctx->opaque;
  for (int i = 0;; i++) {
    const AVCodecHWConfig *config = avcodec_get_hw_config(decoder, i);
    if (!config) {
      LPMS_WARN("Decoder does not support hw decoding");
      return AV_PIX_FMT_NONE;
    }
    if (config->methods & AV_CODEC_HW_CONFIG_METHOD_HW_DEVICE_CTX &&
        config->device_type == params->hw_type) {
      return  config->pix_fmt;
    }
  }
  return AV_PIX_FMT_NONE;
}

/**
 * Callback to negotiate the pixel format for AVCodecContext.
 */
static enum AVPixelFormat get_hw_pixfmt(AVCodecContext *vc, const enum AVPixelFormat *pix_fmts)
{
  AVHWFramesContext *frames;
  int ret = 0;

  // XXX Ideally this would be auto initialized by the HW device ctx
  //     However the initialization doesn't occur in time to set up filters
  //     So we do it here. Also see avcodec_get_hw_frames_parameters
  av_buffer_unref(&vc->hw_frames_ctx);
  vc->hw_frames_ctx = av_hwframe_ctx_alloc(vc->hw_device_ctx);
  if (!vc->hw_frames_ctx) LPMS_ERR(pixfmt_cleanup, "Unable to allocate hwframe context for decoding");

  frames = (AVHWFramesContext*)vc->hw_frames_ctx->data;
  frames->format = hw2pixfmt(vc);
  frames->sw_format = vc->sw_pix_fmt;
  frames->width = vc->width;
  frames->height = vc->height;

  // May want to allocate extra HW frames if we encounter samples where
  // the defaults are insufficient. Raising this increases GPU memory usage
  // For now, the defaults seems OK.
  //vc->extra_hw_frames = 16 + 1; // H.264 max refs

  ret = av_hwframe_ctx_init(vc->hw_frames_ctx);
  if (AVERROR(ENOSYS) == ret) ret = lpms_ERR_INPUT_PIXFMT; // most likely
  if (ret < 0) LPMS_ERR(pixfmt_cleanup, "Unable to initialize a hardware frame pool");

/*
fprintf(stderr, "selected format: hw %s sw %s\n",
av_get_pix_fmt_name(frames->format), av_get_pix_fmt_name(frames->sw_format));
const enum AVPixelFormat *p;
for (p = pix_fmts; *p != -1; p++) {
fprintf(stderr,"possible format: %s\n", av_get_pix_fmt_name(*p));
}
*/

  return frames->format;

pixfmt_cleanup:
  return AV_PIX_FMT_NONE;
}


int open_audio_decoder(input_params *params, struct input_ctx *ctx)
{
  int ret = 0;
  AVCodec *codec = NULL;
  AVFormatContext *ic = ctx->ic;

  // open audio decoder
  ctx->ai = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &codec, 0);
  if (ctx->da) ; // skip decoding audio
  else if (ctx->ai < 0) {
    LPMS_INFO("No audio stream found in input");
  } else {
    AVCodecContext * ac = avcodec_alloc_context3(codec);
    if (!ac) LPMS_ERR(open_audio_err, "Unable to alloc audio codec");
    if (ctx->ac) LPMS_WARN("An audio context was already open!");
    ctx->ac = ac;
    ret = avcodec_parameters_to_context(ac, ic->streams[ctx->ai]->codecpar);
    if (ret < 0) LPMS_ERR(open_audio_err, "Unable to assign audio params");
    ret = avcodec_open2(ac, codec, NULL);
    if (ret < 0) LPMS_ERR(open_audio_err, "Unable to open audio decoder");
  }

  return 0;

open_audio_err:
  free_input(ctx);
  return ret;
}

int open_video_decoder(input_params *params, struct input_ctx *ctx)
{
  int ret = 0;
  AVCodec *codec = NULL;
  AVFormatContext *ic = ctx->ic;

  // open video decoder
  ctx->vi = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (ctx->dv) ; // skip decoding video
  else if (ctx->vi < 0) {
    LPMS_WARN("No video stream found in input");
  } else {
    if (AV_HWDEVICE_TYPE_CUDA == params->hw_type) {
      if (AV_CODEC_ID_H264 != codec->id) {
        ret = lpms_ERR_INPUT_CODEC;
        LPMS_ERR(open_decoder_err, "Non H264 codec detected in input");
      }
      AVCodec *c = avcodec_find_decoder_by_name("h264_cuvid");
      if (c) codec = c;
      else LPMS_WARN("Nvidia decoder not found; defaulting to software");
      if (AV_PIX_FMT_YUV420P != ic->streams[ctx->vi]->codecpar->format &&
          AV_PIX_FMT_YUVJ420P != ic->streams[ctx->vi]->codecpar->format) {
        // TODO check whether the color range is truncated if yuvj420p is used
        ret = lpms_ERR_INPUT_PIXFMT;
        LPMS_ERR(open_decoder_err, "Non 4:2:0 pixel format detected in input");
      }
    }
    AVCodecContext *vc = avcodec_alloc_context3(codec);
    if (!vc) LPMS_ERR(open_decoder_err, "Unable to alloc video codec");
    ctx->vc = vc;
    ret = avcodec_parameters_to_context(vc, ic->streams[ctx->vi]->codecpar);
    if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to assign video params");
    vc->opaque = (void*)ctx;
    // XXX Could this break if the original device falls out of scope in golang?
    if (params->hw_type != AV_HWDEVICE_TYPE_NONE) {
      // First set the hw device then set the hw frame
      ret = av_hwdevice_ctx_create(&ctx->hw_device_ctx, params->hw_type, params->device, NULL, 0);
      if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to open hardware context for decoding")
      ctx->hw_type = params->hw_type;
      vc->hw_device_ctx = av_buffer_ref(ctx->hw_device_ctx);
      vc->get_format = get_hw_pixfmt;
    }
    vc->pkt_timebase = ic->streams[ctx->vi]->time_base;
    ret = avcodec_open2(vc, codec, NULL);
    if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to open video decoder");
  }

  return 0;

open_decoder_err:
  free_input(ctx);
  return ret;
}

int open_input(input_params *params, struct input_ctx *ctx)
{
  AVFormatContext *ic   = NULL;
  char *inp = params->fname;
  int ret = 0;

  // open demuxer
  ret = avformat_open_input(&ic, inp, NULL, NULL);
  if (ret < 0) LPMS_ERR(open_input_err, "demuxer: Unable to open input");
  ctx->ic = ic;
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) LPMS_ERR(open_input_err, "Unable to find input info");
  ret = open_video_decoder(params, ctx);
  if (ret < 0) LPMS_ERR(open_input_err, "Unable to open video decoder")
  ret = open_audio_decoder(params, ctx);
  if (ret < 0) LPMS_ERR(open_input_err, "Unable to open audio decoder")
  ctx->last_frame_v = av_frame_alloc();
  if (!ctx->last_frame_v) LPMS_ERR(open_input_err, "Unable to alloc last_frame_v");
  ctx->last_frame_a = av_frame_alloc();
  if (!ctx->last_frame_a) LPMS_ERR(open_input_err, "Unable to alloc last_frame_a");

  return 0;

open_input_err:
  LPMS_INFO("Freeing input based on OPEN INPUT error");
  free_input(ctx);
  return ret;
}

void free_input(struct input_ctx *inctx)
{
  if (inctx->ic) avformat_close_input(&inctx->ic);
  if (inctx->vc) {
    if (inctx->vc->hw_device_ctx) av_buffer_unref(&inctx->vc->hw_device_ctx);
    avcodec_free_context(&inctx->vc);
  }
  if (inctx->ac) avcodec_free_context(&inctx->ac);
  if (inctx->hw_device_ctx) av_buffer_unref(&inctx->hw_device_ctx);
  if (inctx->last_frame_v) av_frame_free(&inctx->last_frame_v);
  if (inctx->last_frame_a) av_frame_free(&inctx->last_frame_a);
}

