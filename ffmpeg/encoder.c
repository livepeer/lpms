#include "encoder.h"

#include <libavcodec/avcodec.h>

static int encode(AVCodecContext* encoder, AVFrame *frame, struct output_ctx* octx, AVStream* ost) {
#define encode_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) { fprintf(stderr, "should not happen\n"); ret = AVERROR(ENOMEM); } \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s\n", msg, errstr); \
  goto encode_cleanup; \
}

  int ret = 0;
  AVPacket pkt = {0};

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type && frame) {
    if (!octx->res->frames) {
      frame->pict_type = AV_PICTURE_TYPE_I;
    }
    octx->res->frames++;
    octx->res->pixels += encoder->width * encoder->height;
  }


  // We don't want to send NULL frames for HW encoding
  // because that closes the encoder: not something we want
  if (AV_HWDEVICE_TYPE_NONE == octx->hw_type || frame) {
    ret = avcodec_send_frame(encoder, frame);
    if (AVERROR_EOF == ret) ; // continue ; drain encoder
    else if (ret < 0) encode_err("Error sending frame to encoder");
  }

  if (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type &&
      AV_HWDEVICE_TYPE_CUDA == octx->hw_type && !frame) {
    avcodec_flush_buffers(encoder);
  }

  while (1) {
    av_init_packet(&pkt);
    ret = avcodec_receive_packet(encoder, &pkt);
    if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) goto encode_cleanup;
    if (ret < 0) encode_err("Error receiving packet from encoder\n");
    ret = mux(&pkt, encoder->time_base, octx, ost);
    if (ret < 0) goto encode_cleanup;
    av_packet_unref(&pkt);
  }

encode_cleanup:
  av_packet_unref(&pkt);
  return ret;

#undef encode_err
}

int mux(AVPacket *pkt, AVRational tb, struct output_ctx *octx, AVStream *ost)
{
  pkt->stream_index = ost->index;
  if (av_cmp_q(tb, ost->time_base)) {
    av_packet_rescale_ts(pkt, tb, ost->time_base);
  }

  // drop any preroll audio. may need to drop multiple packets for multichannel
  // XXX this breaks if preroll isn't exactly one AVPacket or drop_ts == 0
  //     hasn't been a problem in practice (so far)
  if (AVMEDIA_TYPE_AUDIO == ost->codecpar->codec_type) {
      if (octx->drop_ts == AV_NOPTS_VALUE) octx->drop_ts = pkt->pts;
      if (pkt->pts && pkt->pts == octx->drop_ts) return 0;
  }

  return av_interleaved_write_frame(octx->oc, pkt);
}

int process_out(struct input_ctx *ictx, struct output_ctx *octx, AVCodecContext *encoder, AVStream *ost,
  struct filter_ctx *filter, AVFrame *inf)
{
#define proc_err(msg) { \
  char errstr[AV_ERROR_MAX_STRING_SIZE] = {0}; \
  if (!ret) { fprintf(stderr, "u done messed up\n"); ret = AVERROR(ENOMEM); } \
  if (ret < -1) av_strerror(ret, errstr, sizeof errstr); \
  fprintf(stderr, "%s: %s\n", msg, errstr); \
  goto proc_cleanup; \
}
  int ret = 0;

  if (!encoder) proc_err("Trying to transmux; not supported")

  if (!filter || !filter->active) {
    // No filter in between decoder and encoder, so use input frame directly
    return encode(encoder, inf, octx, ost);
  }

  int is_video = (AVMEDIA_TYPE_VIDEO == ost->codecpar->codec_type);
  ret = filtergraph_write(inf, ictx, octx, filter, is_video);
  if (ret < 0) goto proc_cleanup;

  while (1) {
    // Drain the filter. Each input frame may have multiple output frames
    AVFrame *frame = filter->frame;
    ret = filtergraph_read(ictx, octx, filter, is_video);
    if (ret == lpms_ERR_FILTER_FLUSHED) continue;
    else if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) {
      // no frame returned from filtergraph
      // proceed only if the input frame is a flush (inf == null)
      if (inf) return ret;
      frame = NULL;
    } else if (ret < 0) goto proc_cleanup;

    // Set GOP interval if necessary
    if (is_video && octx->gop_pts_len && frame && frame->pts >= octx->next_kf_pts) {
        frame->pict_type = AV_PICTURE_TYPE_I;
        octx->next_kf_pts = frame->pts + octx->gop_pts_len;
    }
    ret = encode(encoder, frame, octx, ost);
    av_frame_unref(frame);
    // For HW we keep the encoder open so will only get EAGAIN.
    // Return EOF in place of EAGAIN for to terminate the flush
    if (frame == NULL && AV_HWDEVICE_TYPE_NONE != octx->hw_type &&
        AVERROR(EAGAIN) == ret && !inf) return AVERROR_EOF;
    if (frame == NULL) return ret;
  }

proc_cleanup:
  return ret;
#undef proc_err
}

