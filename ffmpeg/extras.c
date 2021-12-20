#include "extras.h"
#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <unistd.h>
#include <time.h>
#include <pthread.h>


//
// Segmenter
//

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char* seg_time, char *seg_start)
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
  AVPacket *pkt         = NULL;
  int64_t prev_ts[2]    = {AV_NOPTS_VALUE, AV_NOPTS_VALUE};
  int stream_map[2]     = {-1, -1};
  int got_video_kf      = 0;

  ret = avformat_open_input(&ic, listen, NULL, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to open input\n");
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) r2h_err("segmenter: Unable to find any input streams\n");

  ofmt = av_guess_format(NULL, outf, NULL);
  if (!ofmt) r2h_err("Could not deduce output format from file extension\n");
  ret = avformat_alloc_output_context2(&oc, ofmt, NULL, outf);
  if (ret < 0) r2h_err("Unable to allocate output context\n");

  // XXX accommodate cases where audio or video is empty
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
  av_dict_set(&md, "start_number", seg_start, 0);
  av_dict_set(&md, "hls_flags", "delete_segments", 0);
  ret = avformat_write_header(oc, &md);
  if (ret < 0) r2h_err("Error writing header\n");

  pkt = av_packet_alloc();
  if (!pkt) r2h_err("Error allocating packet\n");
  while (1) {
    ret = av_read_frame(ic, pkt);
    if (ret == AVERROR_EOF) {
      av_interleaved_write_frame(oc, NULL); // flush
      break;
    } else if (ret < 0) r2h_err("Error reading\n");
    // rescale timestamps
    if (pkt->stream_index == stream_map[0]) pkt->stream_index = 0;
    else if (pkt->stream_index == stream_map[1]) pkt->stream_index = 1;
    else goto r2hloop_end;
    ist = ic->streams[stream_map[pkt->stream_index]];
    ost = oc->streams[pkt->stream_index];
    int64_t dts_next = pkt->dts, dts_prev = prev_ts[pkt->stream_index];
    if (oc->streams[pkt->stream_index]->codecpar->codec_type == AVMEDIA_TYPE_VIDEO &&
        AV_NOPTS_VALUE == dts_prev &&
        (pkt->flags & AV_PKT_FLAG_KEY)) got_video_kf = 1;
    if (!got_video_kf) goto r2hloop_end; // skip everyting until first video KF
    if (AV_NOPTS_VALUE == dts_prev) dts_prev = dts_next;
    else if (dts_next <= dts_prev) goto r2hloop_end; // drop late packets
    pkt->pts = av_rescale_q_rnd(pkt->pts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    pkt->dts = av_rescale_q_rnd(pkt->dts, ist->time_base, ost->time_base,
        AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX);
    if (!pkt->duration) pkt->duration = dts_next - dts_prev;
    pkt->duration = av_rescale_q(pkt->duration, ist->time_base, ost->time_base);
    prev_ts[pkt->stream_index] = dts_next;
    // write the thing
    ret = av_interleaved_write_frame(oc, pkt);
    if (ret < 0) r2h_err("segmenter: Unable to write output frame\n");
r2hloop_end:
    av_packet_unref(pkt);
  }
  ret = av_write_trailer(oc);
  if (ret < 0) r2h_err("segmenter: Unable to write trailer\n");

handle_r2h_err:
  if (errstr) fprintf(stderr, "%s", errstr);
  if (pkt) av_packet_free(&pkt);
  if (ic) avformat_close_input(&ic);
  if (oc) avformat_free_context(oc);
  if (md) av_dict_free(&md);
  return ret == AVERROR_EOF ? 0 : ret;
}

__thread char *log_ctx_local = NULL;

static void replace_new_lines(char *str) {
  char c;
  while ((c = *str)) {
    if (c == '\n' && *(str+1)) {
      *str = ' ';
    }
    str++;
  }
}

static FILE* log_out = NULL;
static pthread_mutex_t mutex = PTHREAD_MUTEX_INITIALIZER;

static void log_callback_report(void *ptr, int level, const char *fmt, va_list vl)
{
    va_list vl2;
    char line1[1024];
    char line2[2048];
    static int print_prefix = 1;

    if (level > av_log_get_level())
        return;

    va_copy(vl2, vl);

    pthread_mutex_lock(&mutex);

    av_log_format_line(ptr, level, fmt, vl2, line1, sizeof(line1), &print_prefix);
    va_end(vl2);
    if (log_ctx_local) {
      replace_new_lines(line1);
      snprintf(line2, sizeof line2, "%s %s", log_ctx_local, line1);
      fputs(line2, log_out);
    } else {
      fputs(line1, log_out);
    }
    pthread_mutex_unlock(&mutex);
}

void lpms_log_deinit() {
  fclose(log_out);
}

// installs custom log callback
void lpms_log_init(int log_out_fd) {
  if (log_out_fd) {
    // use for testing only
    log_out = fdopen(log_out_fd, "wr");
    if (log_out == NULL) {
      log_out = stderr;
    }
  } else {
    log_out = stderr;
  }
  av_log_set_callback(log_callback_report);
}

// used in tests
void lpms_log_test(char *log_ctx) {
  log_ctx_local = log_ctx;
  av_log(NULL, AV_LOG_INFO, "context logging test second=%d\n", 0);
  usleep(100000);
  av_log(NULL, AV_LOG_INFO, "context logging test second=%d\n", 1);
  usleep(100000);
  av_log(NULL, AV_LOG_INFO, "context logging test second=%d\n", 2);
  usleep(100000);
  av_log(NULL, AV_LOG_INFO, "context logging test second=%d\n", 3);
  log_ctx_local = NULL;
}

//
// Bypass Check
// this is needed to handle streams that have first few segments that are
// audio-only (i.e. have a video stream but no frames)
// returns: 0 if both audio/video streams valid
//          1 for video with 0-frame, that needs bypass
//          <0 invalid stream(s) or internal error
//
int lpms_is_bypass_needed(char *log_ctx, char *fname)
{
  log_ctx_local = log_ctx;

  AVFormatContext *ic = NULL;
  int ret = 0, vstream = 0, astream = 0;

  ret = avformat_open_input(&ic, fname, NULL, NULL);
  if (ret < 0) { ret = -1; goto close_format_context; }
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) { ret = -1; goto close_format_context; }

  vstream = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, NULL, 0);
  astream = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, NULL, 0);
  if (vstream >= 0 && astream >= 0) {
      if (AV_PIX_FMT_NONE == ic->streams[vstream]->codecpar->format &&
          0 == ic->streams[vstream]->codecpar->height) {
          // no valid pixel format and picture height => needs bypass
          ret = 1;
      } else {
          // no bypass needed if video stream is valid
          ret = 0;
      }
  } else {
      // one of audio or video streams not present at all, won't bypass
      ret = -1;
  }
close_format_context:
  if (ic) avformat_close_input(&ic);
  log_ctx_local = NULL;
  return ret;
}

// compare two signature files whether those matches or not.
// @param signpath1        full path of the first signature file.
// @param signpath2        full path of the second signature file.
// @return  <0: error 0: no matchiing 1: partial matching 2: whole matching.

int lpms_compare_sign_bypath(char *signpath1, char *signpath2)
{
  int ret = avfilter_compare_sign_bypath(signpath1, signpath2);
  return ret;
}
// compare two signature buffers whether those matches or not.
// @param signbuf1        the pointer of the first signature buffer.
// @param signbuf2        the pointer of the second signature buffer.
// @param len1            the length of the first signature buffer.
// @param len2            the length of the second signature buffer.
// @return  <0: error =0: no matchiing 1: partial matching 2: whole matching.
int lpms_compare_sign_bybuffer(void *buffer1, int len1, void *buffer2, int len2)
{
  int ret = avfilter_compare_sign_bybuff(buffer1, len1, buffer2, len2);
  return ret;
}
