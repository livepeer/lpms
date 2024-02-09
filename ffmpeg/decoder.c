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

static int send_first_pkt(struct input_ctx *ictx)
{
  if (ictx->flushed) return 0;
  if (!ictx->first_pkt) return lpms_ERR_INPUT_NOKF;

  int ret = avcodec_send_packet(ictx->vc, ictx->first_pkt);
  ictx->sentinel_count++;
  if (ret < 0) {
    LPMS_ERR(packet_cleanup, "Error sending flush packet");
  }
packet_cleanup:
  return ret;
}

int demux_in(struct input_ctx *ictx, AVPacket *pkt)
{
  return av_read_frame(ictx->ic, pkt);
}

int decode_in(struct input_ctx *ictx, AVPacket *pkt, AVFrame *frame, int *stream_index)
{
  int ret = 0;
  AVStream *ist = NULL;
  AVCodecContext *decoder = NULL;

  *stream_index = pkt->stream_index;
  ist = ictx->ic->streams[pkt->stream_index];
  if (ist->index == ictx->vi && ictx->vc) {
    // this is video packet to decode
    decoder = ictx->vc;
  } else if (ist->index == ictx->ai && ictx->ac) {
    // this is audio packet to decode
    decoder = ictx->ac;
  } else if (pkt->stream_index == ictx->vi || pkt->stream_index == ictx->ai || ictx->transmuxing) {
    // MA: this is original code. I think the intention was
    // if (audio or video) AND transmuxing
    // so it is buggy, but nevermind, refactored code will handle things in
    // different way (won't ever call decode on transmuxing channels)
    return 0;
  } else {
    // otherwise this stream is not used for anything
    // TODO: but this is also done in transcode() loop, no?
    av_packet_unref(pkt);
    return 0;
  }

  if (!ictx->first_pkt && pkt->flags & AV_PKT_FLAG_KEY && decoder == ictx->vc) {
    ictx->first_pkt = av_packet_clone(pkt);
    ictx->first_pkt->pts = -1;
  }

  ret = lpms_send_packet(ictx, decoder, pkt);
  if (ret < 0) {
    LPMS_ERR_RETURN("Error sending packet to decoder");
  }
  ret = lpms_receive_frame(ictx, decoder, frame);
  if (ret == AVERROR(EAGAIN)) {
    // This is not really an error. It may be that packet just fed into
    // the decoder may be not enough to complete decoding. Upper level will
    // get next packet and retry
    return lpms_ERR_PACKET_ONLY;
  } else if (ret < 0) {
    LPMS_ERR_RETURN("Error receiving frame from decoder");
  } else {
    return ret;
  }
}

int flush_in(struct input_ctx *ictx, AVFrame *frame, int *stream_index)
{
  int ret = 0;
  // Attempt to read all frames that are remaining within the decoder, starting
  // with video. If there's a nonzero response type, we know there are no more
  // video frames, so continue on to audio.

  // Flush video decoder.
  // To accommodate CUDA, we feed the decoder sentinel (flush) frames, till we
  // get back all sent frames, or we've made SENTINEL_MAX attempts to retrieve
  // buffered frames with no success.
  // TODO this is unnecessary for SW decoding! SW process should match audio
  if (ictx->vc && !ictx->flushed && ictx->pkt_diff > 0) {
    ictx->flushing = 1;
    ret = send_first_pkt(ictx);
    if (ret < 0) {
      ictx->flushed = 1;
      return ret;
    }
    ret = lpms_receive_frame(ictx, ictx->vc, frame);
    *stream_index = ictx->vi;
    // Keep flushing if we haven't received all frames back but stop after SENTINEL_MAX tries.
    if (ictx->pkt_diff != 0 && ictx->sentinel_count <= SENTINEL_MAX && (!ret || ret == AVERROR(EAGAIN))) {
      return ret;
    } else {
      ictx->flushed = 1;
      if (!ret) return ret;
    }
  }
  // Flush audio decoder.
  if (ictx->ac) {
    avcodec_send_packet(ictx->ac, NULL);
    ret = avcodec_receive_frame(ictx->ac, frame);
    *stream_index = ictx->ai;
    if (!ret) return ret;
  }
  return AVERROR_EOF;
}

int process_in(struct input_ctx *ictx, AVFrame *frame, AVPacket *pkt,
               int *stream_index)
{
  int ret = 0;

  av_packet_unref(pkt);

  // Demux next packet
  ret = demux_in(ictx, pkt);
  // See if we got anything
  if (ret == AVERROR_EOF) {
    // no more packets, flush the decoder(s)
    return flush_in(ictx, frame, stream_index);
  } else if (ret < 0) {
    // demuxing error
    LPMS_ERR_RETURN("Unable to read input");
  } else {
    // decode
    return decode_in(ictx, pkt, frame, stream_index);
  }
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
  return frames->format;

pixfmt_cleanup:
  return AV_PIX_FMT_NONE;
}


int open_audio_decoder(input_params *params, struct input_ctx *ctx)
{
  int ret = 0;
  const AVCodec *codec = NULL;
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

char* get_hw_decoder(int ff_codec_id, int hw_type)
{
    switch (hw_type) {
        case AV_HWDEVICE_TYPE_CUDA:
            switch (ff_codec_id) {
                case AV_CODEC_ID_H264:
                    return "h264_cuvid";
                case AV_CODEC_ID_HEVC:
                    return "hevc_cuvid";
                case AV_CODEC_ID_VP8:
                    return "vp8_cuvid";
                case AV_CODEC_ID_VP9:
                    return "vp9_cuvid";
                default:
                    return "";
            }
        case AV_HWDEVICE_TYPE_MEDIACODEC:
            switch (ff_codec_id) {
                case AV_CODEC_ID_H264:
                    return "h264_ni_dec";
                case AV_CODEC_ID_HEVC:
                    return "h265_ni_dec";
                case AV_CODEC_ID_VP8:
                    return "";
                case AV_CODEC_ID_VP9:
                    return "";
                default:
                    return "";
            }
    }
}

int open_video_decoder(input_params *params, struct input_ctx *ctx)
{
  int ret = 0;
  const AVCodec *codec = NULL;
  AVDictionary **opts = NULL;
  AVFormatContext *ic = ctx->ic;
  // open video decoder
  ctx->vi = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &codec, 0);
  if (ctx->dv) ; // skip decoding video
  else if (ctx->vi < 0) {
    LPMS_WARN("No video stream found in input");
  } else {
    if (params->hw_type > AV_HWDEVICE_TYPE_NONE) {
      char* decoder_name = get_hw_decoder(codec->id, params->hw_type);
      if (!*decoder_name) {
        ret = lpms_ERR_INPUT_CODEC;
        LPMS_ERR(open_decoder_err, "Input codec does not support hardware acceleration");
      }
      const AVCodec *c = avcodec_find_decoder_by_name(decoder_name);
      if (c) codec = c;
      else LPMS_WARN("Nvidia decoder not found; defaulting to software");
      if (AV_PIX_FMT_YUV420P != ic->streams[ctx->vi]->codecpar->format &&
          AV_PIX_FMT_YUVJ420P != ic->streams[ctx->vi]->codecpar->format) {
        // TODO check whether the color range is truncated if yuvj420p is used
        ret = lpms_ERR_INPUT_PIXFMT;
        LPMS_ERR(open_decoder_err, "Non 4:2:0 pixel format detected in input");
      }
    } else if (params->video.name && strlen(params->video.name) != 0) {
      // Try to find user specified decoder by name
      const AVCodec *c = avcodec_find_decoder_by_name(params->video.name);
      av_log(NULL, AV_LOG_WARNING, "selecting decoder for, %s\n", params->video.name);
      if (c) codec = c;
      if (params->video.opts) opts = &params->video.opts;
    }
    AVCodecContext *vc = avcodec_alloc_context3(codec);
    if (!vc) LPMS_ERR(open_decoder_err, "Unable to alloc video codec");
    ctx->vc = vc;
    ret = avcodec_parameters_to_context(vc, ic->streams[ctx->vi]->codecpar);
    if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to assign video params");
    vc->opaque = (void*)ctx;
    // XXX Could this break if the original device falls out of scope in golang?
    if (params->hw_type == AV_HWDEVICE_TYPE_CUDA) {
      // First set the hw device then set the hw frame
      ret = av_hwdevice_ctx_create(&ctx->hw_device_ctx, params->hw_type, params->device, NULL, 0);
      if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to open hardware context for decoding")
      vc->hw_device_ctx = av_buffer_ref(ctx->hw_device_ctx);
      vc->get_format = get_hw_pixfmt;
    }
    ctx->hw_type = params->hw_type;
    vc->pkt_timebase = ic->streams[ctx->vi]->time_base;
    av_opt_set(vc->priv_data, "xcoder-params", ctx->xcoderParams, 0);
    ret = avcodec_open2(vc, codec, opts);
    if (ret < 0) LPMS_ERR(open_decoder_err, "Unable to open video decoder");
  }

  return 0;

open_decoder_err:
  free_input(ctx);
  if (ret == AVERROR_UNKNOWN) ret = lpms_ERR_UNRECOVERABLE;
  return ret;
}

int open_input(input_params *params, struct input_ctx *ctx)
{
  AVFormatContext *ic   = NULL;
  char *inp = params->fname;
  int ret = 0;

  ctx->transmuxing = params->transmuxe;
  av_log(NULL, AV_LOG_WARNING, "opening input, hw_type=%d\n", params->hw_type);
  // open demuxer
  ret = avformat_open_input(&ic, inp, NULL, NULL);
  if (ret < 0) LPMS_ERR(open_input_err, "demuxer: Unable to open input");
  ctx->ic = ic;
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) LPMS_ERR(open_input_err, "Unable to find input info");
  if (params->transmuxe == 0) {
    ret = open_video_decoder(params, ctx);
    if (ret < 0) LPMS_ERR(open_input_err, "Unable to open video decoder")
    ret = open_audio_decoder(params, ctx);
    if (ret < 0) LPMS_ERR(open_input_err, "Unable to open audio decoder")
    ctx->last_frame_v = av_frame_alloc();
    if (!ctx->last_frame_v) LPMS_ERR(open_input_err, "Unable to alloc last_frame_v");
    ctx->last_frame_a = av_frame_alloc();
    if (!ctx->last_frame_a) LPMS_ERR(open_input_err, "Unable to alloc last_frame_a");
  }

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

