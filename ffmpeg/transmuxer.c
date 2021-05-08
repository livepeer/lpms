#include "transmuxer.h"
#include "logging.h"
// #include "decoder.h"
#include "filter.h"
// #include "encoder.h"
#include "decoder.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavutil/timestamp.h>

struct transmuxe_thread {
  int initialized;

  struct input_ctx ictx;
  struct output_ctx octx;
};

static void log_packet(const AVFormatContext *fmt_ctx, const AVPacket *pkt,
                       const char *tag) {
  AVRational *time_base = &fmt_ctx->streams[pkt->stream_index]->time_base;

  printf("%s: pts:%s pts_time:%s dts:%s dts_time:%s duration:%s "
         "duration_time:%s stream_index:%d\n",
         tag, av_ts2str(pkt->pts), av_ts2timestr(pkt->pts, time_base),
         av_ts2str(pkt->dts), av_ts2timestr(pkt->dts, time_base),
         av_ts2str(pkt->duration), av_ts2timestr(pkt->duration, time_base),
         pkt->stream_index);
}

int m_open_input(m_input_params *params, struct input_ctx *ctx) {
  AVFormatContext *ic = NULL;
  char *inp = params->fname;
  int ret = 0;

  // open demuxer
  ret = avformat_open_input(&ic, inp, NULL, NULL);
  if (ret < 0)
    LPMS_ERR(open_input_err, "demuxer: Unable to open input");
  ctx->ic = ic;
  for (int i = 0; i < ic->nb_streams; i++) {
    ic->streams[i]->probe_packets = 64;
  }
  LPMS_INFO("open input start 00 ic->max_probe_packets");
  av_log(NULL, AV_LOG_DEBUG, "max probe packetd :%d\n", ic->max_probe_packets);
  // ic->max_analyze_duration = 5000;
  ic->max_probe_packets = 16;
  ic->debug = 1;
  // ic->probesize = 50000;
  av_log(NULL, AV_LOG_DEBUG, "max probe packetd :%d\n", ic->max_probe_packets);
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0)
    LPMS_ERR(open_input_err, "Unable to find input info");

  return 0;

open_input_err:
  LPMS_INFO("Freeing input based on OPEN INPUT error");
  free_input(ctx);
  return ret;
}

int m_open_output(struct output_ctx *octx, struct input_ctx *ictx) {
  int ret = 0, inp_has_stream;

  AVOutputFormat *fmt = NULL;
  AVFormatContext *oc = NULL;
  AVCodecContext *vc = NULL;
  AVCodec *codec = NULL;

  // open muxer
  fmt = av_guess_format(octx->muxer->name, octx->fname, NULL);
  if (!fmt)
    LPMS_ERR(open_output_err, "Unable to guess output format");
  ret = avformat_alloc_output_context2(&oc, fmt, NULL, octx->fname);
  if (ret < 0)
    LPMS_ERR(open_output_err, "Unable to alloc output context");
  octx->oc = oc;
  oc->flags |= AVFMT_FLAG_FLUSH_PACKETS;
  oc->flush_packets = 1;

  for (int i = 0; i < ictx->ic->nb_streams; i++) {
    ret = 0;
    AVStream *st = avformat_new_stream(octx->oc, NULL);
    if (!st)
      LPMS_ERR(open_output_err, "Unable to alloc stream");
    if (octx->fps.den)
      st->avg_frame_rate = octx->fps;
    else
      st->avg_frame_rate = ictx->ic->streams[i]->r_frame_rate;

    AVStream *ist = ictx->ic->streams[i];
    st->time_base = ist->time_base;
    ret = avcodec_parameters_copy(st->codecpar, ist->codecpar);
    if (ret < 0)
      LPMS_ERR(open_output_err, "Error copying params from input stream");
    // Sometimes the codec tag is wonky for some reason, so correct it
    ret = av_codec_get_tag2(octx->oc->oformat->codec_tag,
                            st->codecpar->codec_id, &st->codecpar->codec_tag);
    avformat_transfer_internal_stream_timing_info(octx->oc->oformat, st, ist,
                                                  AVFMT_TBCF_DEMUXER);
  }

  if (!(fmt->flags & AVFMT_NOFILE)) {
    ret = avio_open(&octx->oc->pb, octx->fname, AVIO_FLAG_WRITE);
    if (ret < 0)
      LPMS_ERR(open_output_err, "Error opening output file");
  }

  ret = avformat_write_header(oc, &octx->muxer->opts);
  if (ret < 0)
    LPMS_ERR(open_output_err, "Error writing header");

  return 0;

open_output_err:
  LPMS_WARN("Closing output by error");
  free_output(octx);
  return ret;
}

int transmuxe(struct transmuxe_thread *h, m_input_params *inp,
              m_output_params *params, output_results *results) {
  int ret = 0, i = 0;
  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *octx = &h->octx;
  AVPacket ipkt = {0};

  if (!inp)
    LPMS_ERR(transcode_cleanup, "Missing input params")

  // by default we re-use decoder between segments of same stream
  // unless we are using SW deocder and had to re-open IO or demuxer
  if (!ictx->ic) {
    LPMS_ERR(transcode_cleanup, "Should be opened now")
  }

  // first segment of a stream, need to initalize output HW context
  // XXX valgrind this line up
  if (!h->initialized) {
    // populate output contexts
    octx->fname = params->fname;
    octx->muxer = &params->muxer;
    // octx->res = results;
    LPMS_INFO("output init 1");

    ret = m_open_output(octx, ictx);
    if (ret < 0)
      LPMS_ERR(transcode_cleanup, "Unable to open output");
  }

  av_init_packet(&ipkt);

  while (1) {
    AVStream *in_stream, *out_stream;
    ret = av_read_frame(ictx->ic, &ipkt);
    if (ret == AVERROR_EOF)
      break;
    if (ret == AVERROR(EAGAIN)) {
      LPMS_INFO("===> read frame again");
      continue;
    }
    if (ret < 0)
      LPMS_ERR(transcode_cleanup, "Unable to read frame");
    if (ictx->discontinuity) {
      // calc pts diff
      ictx->pts_diff = ictx->last_pts + ictx->last_duration - ipkt.pts;
      ictx->discontinuity = 0;
    }

    // log_packet(ictx->ic, &ipkt, "oin");
    ipkt.pts += ictx->pts_diff;
    ipkt.dts += ictx->pts_diff;

    if (ipkt.stream_index == 0) {
      ictx->last_pts = ipkt.pts;
      if (ipkt.duration) {
        ictx->last_duration = ipkt.duration;
      }
    }
    in_stream = ictx->ic->streams[ipkt.stream_index];
    out_stream = octx->oc->streams[ipkt.stream_index];

    // log_packet(ictx->ic, &ipkt, "cin");

    // av_log(NULL, AV_LOG_INFO, "%s:%d] %s %lld dts %lld dur %lld frame dur
    // %lld\n", __FILE__, __LINE__,
    //   "got packet with pts", ipkt.pts, ipkt.dts, ipkt.duration,
    //   ictx->duration);

    // pkt = av_packet_clone(&ipkt);
    // if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for
    // copy");
    ipkt.pts =
        av_rescale_q_rnd(ipkt.pts, in_stream->time_base, out_stream->time_base,
                         AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    ipkt.dts =
        av_rescale_q_rnd(ipkt.dts, in_stream->time_base, out_stream->time_base,
                         AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    ipkt.duration = av_rescale_q(ipkt.duration, in_stream->time_base,
                                 out_stream->time_base);
    ipkt.pos = -1;
    // log_packet(octx->oc, &ipkt, "out");
    ret = av_interleaved_write_frame(octx->oc, &ipkt);
    if (AVERROR(EAGAIN)) {
      // LPMS_WARN("Got EGAIN on av_interleaved_write_frame");
      ret = 0;
    }
    if (AVERROR_EOF == ret) {
      // LPMS_WARN("Got EOF on av_interleaved_write_frame");
      LPMS_ERR(transcode_cleanup, "Got EOF on av_interleaved_write_frame");
    }
    if (ret < 0)
      LPMS_ERR(transcode_cleanup, "Error muxing");
    results->frames++;

    av_log(NULL, AV_LOG_DEBUG, "frames count :%d\n", results->frames);

  whileloop_end:
    av_packet_unref(&ipkt);
  }

  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  // return av_write_trailer(octx->oc);
  free_input(ictx);
  return 0;

transcode_cleanup:
  free_input(ictx);
  ictx->flushed = 0;
  ictx->flushing = 0;
  ictx->pkt_diff = 0;
  ictx->sentinel_count = 0;
  av_packet_unref(&ipkt); // needed for early exits
  return ret == AVERROR_EOF ? 0 : ret;
}

int lpms_transmuxe(m_input_params *inp, m_output_params *params,
                   output_results *results) {
  int ret = 0;
  struct transmuxe_thread *h = inp->handle;

  // populate input context
  ret = m_open_input(inp, &h->ictx);
  if (ret < 0) {
    return ret;
  }

  ret = transmuxe(h, inp, params, results);
  h->initialized = 1;

  return ret;
}

struct transmuxe_thread *lpms_transmuxe_new() {
  struct transmuxe_thread *h = malloc(sizeof(struct transmuxe_thread));
  if (!h)
    return NULL;
  memset(h, 0, sizeof *h);
  return h;
}

void lpms_transmuxe_discontinuity(struct transmuxe_thread *handle) {
  if (!handle)
    return;
  handle->ictx.discontinuity = 1;
}

void lpms_transmuxe_stop(struct transmuxe_thread *handle) {
  // not threadsafe as-is; calling function must ensure exclusivity!

  int i;

  if (!handle)
    return;

  free_input(&handle->ictx);

  if (handle->octx.oc) {
    av_write_trailer(handle->octx.oc);
    if (!(handle->octx.oc->oformat->flags & AVFMT_NOFILE) &&
        handle->octx.oc->pb) {
      avio_closep(&handle->octx.oc->pb);
    }
    avformat_free_context(handle->octx.oc);
    handle->octx.oc = NULL;
  }

  free(handle);
}
