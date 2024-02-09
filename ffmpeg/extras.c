#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>
#include <libavfilter/avfilter.h>
#include <stdbool.h>
#include <libavutil/md5.h>
#include "extras.h"
#include "logging.h"

#define MAX_AMISMATCH 10
#define INC_MD5_COUNT 300
#define MAX_MD5_COUNT 30000
#define MD5_SIZE 16   //sizeof(int)*4 byte

struct match_info {
  int       width;
  int       height;
  uint64_t  bit_rate;
  int       packetcount;  //video total packet count
  uint64_t  timestamp;    //XOR sum of avpacket pts  
  int       md5allocsize;
  int       apacketcount; //audio packet count
  int       *pmd5array;
};

struct buffer_data {
    uint8_t *ptr;
    size_t size; ///< size left in the buffer
};

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
  const AVOutputFormat *ofmt  = NULL;
  AVStream *ist         = NULL;
  AVStream *ost         = NULL;
  AVDictionary *md      = NULL;
  const AVCodec *codec        = NULL;
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

#define GET_CODEC_INTERNAL_ERROR -1
#define GET_CODEC_OK 0
#define GET_CODEC_NEEDS_BYPASS 1
#define GET_CODEC_STREAMS_MISSING 2
//
// Gets codec names for best video and audio streams
// Also detects if bypass is needed for first few segments that are
// audio-only (i.e. have a video stream but no frames)
// If codec name can't be determined string is truncated to 0 size
// returns: 0 if both audio/video streams valid
//          1 for video with 0-frame, that needs bypass
//          2 if audio or video stream is missing
//          <0 invalid stream(s) or internal error
//
int lpms_get_codec_info(char *fname, pcodec_info out)
{
#define MIN(x, y) (((x) < (y)) ? (x) : (y))
  AVFormatContext *ic = NULL;
  const AVCodec *ac, *vc;
  int ret = GET_CODEC_OK, vstream = 0, astream = 0;

  ret = avformat_open_input(&ic, fname, NULL, NULL);
  if (ret < 0) { ret = GET_CODEC_INTERNAL_ERROR; goto close_format_context; }
  ret = avformat_find_stream_info(ic, NULL);
  if (ret < 0) { ret = GET_CODEC_INTERNAL_ERROR; goto close_format_context; }

  vstream = av_find_best_stream(ic, AVMEDIA_TYPE_VIDEO, -1, -1, &vc, 0);
  astream = av_find_best_stream(ic, AVMEDIA_TYPE_AUDIO, -1, -1, &ac, 0);
  bool audio_present = astream >= 0;
  bool video_present = vstream >= 0;
  if(!audio_present && !video_present) {
    // instead of returning -1
    ret = GET_CODEC_STREAMS_MISSING;
  }
  // Return
  if (video_present && vc->name) {
      strncpy(out->video_codec, vc->name, MIN(strlen(out->video_codec), strlen(vc->name))+1);
      // If video track is present extract pixel format info
      out->pixel_format         = ic->streams[vstream]->codecpar->format;
      bool pixel_format_missing = AV_PIX_FMT_NONE == out->pixel_format;
      bool no_picture_height    = 0 == ic->streams[vstream]->codecpar->height;
      if(audio_present && pixel_format_missing && no_picture_height) {
        ret = GET_CODEC_NEEDS_BYPASS;
      }
      out->width  = ic->streams[vstream]->codecpar->width;
      out->height = ic->streams[vstream]->codecpar->height;
  } else {
      // Indicate failure to extract video codec from given container
      out->video_codec[0] = 0;
  }
  if (audio_present && ac->name) {
      strncpy(out->audio_codec, ac->name, MIN(strlen(out->audio_codec), strlen(ac->name))+1);
  } else {
      // Indicate failure to extract audio codec from given container
      out->audio_codec[0] = 0;
  }
#undef MIN
close_format_context:
  if (ic) avformat_close_input(&ic);
  return ret;
}

//// compare two signature files whether those matches or not.
//// @param signpath1        full path of the first signature file.
//// @param signpath2        full path of the second signature file.
//// @return  <0: error 0: no matchiing 1: partial matching 2: whole matching.

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

static int get_filesize(const char *filename)
{
    int fileLength = 0;
    FILE *f = NULL;
    f = fopen(filename, "rb");
    if(f != NULL) {
        fseek(f, 0, SEEK_END);
        fileLength = ftell(f);
        fclose(f);
    }
    return fileLength;
}

static uint8_t * get_filebuffer(const char *filename, int* fileLength)
{
    FILE *f = NULL;
    unsigned int readLength, paddedLength = 0;
    uint8_t *buffer = NULL;

    //check input parameters
    if (strlen(filename) <= 0) return buffer;
    f = fopen(filename, "rb");
    if (f == NULL) {
        av_log(NULL, AV_LOG_ERROR, "Could not open the file %s\n", filename);
        return buffer;
    }
    *fileLength = get_filesize(filename);
    if(*fileLength > 0) {
        // Cast to float is necessary to avoid int division
        paddedLength = ceil(*fileLength / (float)AV_INPUT_BUFFER_PADDING_SIZE)*AV_INPUT_BUFFER_PADDING_SIZE + AV_INPUT_BUFFER_PADDING_SIZE;
        buffer = (uint8_t*)av_calloc(paddedLength, sizeof(uint8_t));
        if (!buffer) {
            av_log(NULL, AV_LOG_ERROR, "Could not allocate memory for reading signature file\n");
            fclose(f);
            return NULL;
        }
        // Read entire file into memory
        readLength = fread(buffer, sizeof(uint8_t), *fileLength, f);
        if(readLength != *fileLength) {
            av_log(NULL, AV_LOG_ERROR, "Could not read the file %s\n", filename);
            free(buffer);
            buffer = NULL;
        }
    }
    fclose(f);
    return buffer;
}

static int read_packet(void *opaque, uint8_t *buf, int buf_size)
{
    struct buffer_data *bd = (struct buffer_data *)opaque;
    buf_size = FFMIN(buf_size, bd->size);

    if (!buf_size)
        return AVERROR_EOF;
    /* copy internal buffer data to buf */
    memcpy(buf, bd->ptr, buf_size);
    bd->ptr  += buf_size;
    bd->size -= buf_size;

    return buf_size;
}

static int get_matchinfo(void *buffer, int len, struct match_info* info)
{
  int ret = 0;
  AVFormatContext* ifmt_ctx = NULL;
  AVIOContext *avio_in = NULL;
  AVPacket *packet = NULL;
  int audioid = -1;
  int md5tmp[4];
  uint8_t *avio_ctx_buffer = NULL;
  size_t  avio_ctx_buffer_size = 4096;
  struct buffer_data bd = { 0 };
  //initialize matching information
  memset(info, 0x00, sizeof(struct match_info));

   /* fill opaque structure used by the AVIOContext read callback */
  bd.ptr  = buffer;
  bd.size = len;

  avio_ctx_buffer = av_malloc(avio_ctx_buffer_size);
  if (!avio_ctx_buffer) {
        ret = AVERROR(ENOMEM);
        LPMS_ERR(clean, "Error allocating buffer");
  }

  avio_in = avio_alloc_context(avio_ctx_buffer, avio_ctx_buffer_size, 0, &bd, &read_packet, NULL, NULL);
  if (!avio_ctx_buffer) {
        ret = AVERROR(ENOMEM);
        LPMS_ERR(clean, "Error allocating context");
  }
  ifmt_ctx = avformat_alloc_context();
  if (!ifmt_ctx) {
        ret = AVERROR(ENOMEM);
        LPMS_ERR(clean, "Error allocating avformat context");
  }
  ifmt_ctx->pb = avio_in;
  ifmt_ctx->flags = AVFMT_FLAG_CUSTOM_IO;

  if ((ret = avformat_open_input(&ifmt_ctx, "", NULL, NULL)) < 0) {
        LPMS_ERR(clean, "Cannot open input video file\n");
  }

  if ((ret = avformat_find_stream_info(ifmt_ctx, NULL)) < 0) {
        LPMS_ERR(clean, "Cannot find stream information\n");
  }

  for (int i = 0; i < ifmt_ctx->nb_streams; i++) {
    AVStream *stream;
    stream = ifmt_ctx->streams[i];
    AVCodecParameters *in_codecpar = stream->codecpar;
    if (in_codecpar->codec_type == AVMEDIA_TYPE_VIDEO) {
      info->width = in_codecpar->width;
      info->height = in_codecpar->height;
      info->bit_rate = in_codecpar->bit_rate;
    }
    else if (in_codecpar->codec_type == AVMEDIA_TYPE_AUDIO) {
      audioid = i;
    }
  }
  packet = av_packet_alloc();
  if (!packet) LPMS_ERR(clean, "Error allocating packet");
  while (1) {
    ret = av_read_frame(ifmt_ctx, packet);
    if (ret == AVERROR_EOF) {
      ret = 0;
      break;
    }
    else if (ret < 0) {
      LPMS_ERR(clean, "Unable to read input");
    }
    info->packetcount++;
    info->timestamp ^= packet->pts;
    if (packet->stream_index == audioid && packet->size > 0) {
      if (info->apacketcount < MAX_MD5_COUNT) {
          info->apacketcount++;
          if (info->apacketcount > info->md5allocsize) {
            info->md5allocsize += INC_MD5_COUNT;
            if (info->pmd5array == NULL) {
              info->pmd5array = (int*)av_malloc(info->md5allocsize*MD5_SIZE);
            } else {
              int* tmp = info->pmd5array;
              info->pmd5array = (int*)av_malloc(info->md5allocsize*MD5_SIZE);
              memcpy(info->pmd5array, tmp, (info->md5allocsize-INC_MD5_COUNT)*MD5_SIZE);
              av_free(tmp);
            }
          }
          int* pint = info->pmd5array + (info->apacketcount-1) * 4;
          av_md5_sum((uint8_t*)(pint), packet->data, packet->size);
      }
    }
    av_packet_unref(packet);
  }

clean:
  if(packet)
    av_packet_free(&packet);
  /* note: the internal buffer could have changed, and be != avio_ctx_buffer */
  if(avio_in)
    av_freep(&avio_in->buffer);
  avio_context_free(&avio_in);
  avformat_close_input(&ifmt_ctx);
  return ret;
}
// check validity for audio md5 data
bool is_valid_md5data(struct match_info* info1, struct match_info *info2)
{
#define max(a, b) ((a) > (b) ? (a) : (b))
#define min(a, b) ((a) < (b) ? (a) : (b))
  struct match_info* first = info1->apacketcount < info2->apacketcount? info1: info2;
  struct match_info* second = info1->apacketcount >= info2->apacketcount? info1: info2;
  int packetdiff = second->apacketcount - first->apacketcount;
  if (packetdiff > MAX_AMISMATCH) return false;
  int matchingcount = 0;
  for (int i = 0; i < first->apacketcount; i++) {
    int scanscope = packetdiff + 1;
    int *psrcmpd = first->pmd5array + (i*4);
    int nstart = max(0,i-scanscope);
    int nend = min(second->apacketcount,i+scanscope);
    for (int j = nstart; j < nend; j++) {
      if(memcmp(psrcmpd, second->pmd5array + (j*4), MD5_SIZE) == 0){
        matchingcount++;
        break;
      }
    }
  }
  int realdiff = first->apacketcount - matchingcount;
  return realdiff < MAX_AMISMATCH ? true: false;
}
// compare two video buffers whether those matches or not.
// @param buffer1         the pointer of the first video buffer.
// @param buffer2         the pointer of the second video buffer.
// @param len1            the length of the first video buffer.
// @param len2            the length of the second video buffer.
// @return  <0: error =0: matching 1: no matching
int lpms_compare_video_bybuffer(void *buffer1, int len1, void *buffer2, int len2)
{
  int ret = 0;
  struct match_info info1 = {0,}, info2 = {0,};

  ret = get_matchinfo(buffer1,len1,&info1);
  if(ret < 0) goto clean;

  ret = get_matchinfo(buffer2,len2,&info2);
  if(ret < 0) goto clean;
  //compare two matching information
  if (info1.width != info2.width || info1.height != info2.height || !is_valid_md5data(&info1, &info2)) {
      ret = 1;
  }
clean:
  if(info1.pmd5array) av_free(info1.pmd5array);
  if(info2.pmd5array) av_free(info2.pmd5array);

  return ret;
}

// compare two video files whether those matches or not.
// @param vpath1        full path of the first video file.
// @param vpath2        full path of the second video file.
// @return  <0: error =0: matching 1: no matching
int lpms_compare_video_bypath(char *vpath1, char *vpath2)
{
  int ret = 0;
  int len1, len2;
  uint8_t *buffer1, *buffer2;
  buffer1 = get_filebuffer(vpath1, &len1);
  if(buffer1 == NULL) return AVERROR(ENOMEM);
  buffer2 = get_filebuffer(vpath2, &len2);
  if(buffer2 == NULL) {
      av_freep(&buffer1);
      return AVERROR(ENOMEM);
  }
  ret = lpms_compare_video_bybuffer(buffer1, len1, buffer2, len2);

  if(buffer1 != NULL)
      av_freep(&buffer1);
  if(buffer2 != NULL)
      av_freep(&buffer2);

  return ret;
}
