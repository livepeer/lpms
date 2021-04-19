#include "transcoder.h"
#include "decoder.h"
#include "filter.h"
#include "encoder.h"
#include "logging.h"

#include <libavcodec/avcodec.h>
#include <libavformat/avformat.h>

// Not great to appropriate internal API like this...
const int lpms_ERR_INPUT_PIXFMT = FFERRTAG('I','N','P','X');
const int lpms_ERR_INPUT_CODEC = FFERRTAG('I','N','P','C');
const int lpms_ERR_FILTERS = FFERRTAG('F','L','T','R');
const int lpms_ERR_PACKET_ONLY = FFERRTAG('P','K','O','N');
const int lpms_ERR_FILTER_FLUSHED = FFERRTAG('F','L','F','L');
const int lpms_ERR_OUTPUTS = FFERRTAG('O','U','T','P');
const int lpms_ERR_DTS = FFERRTAG('-','D','T','S');

//
//  Notes on transcoder internals:
//
//  Transcoding follows the typical process of the FFmpeg API:
//    read/demux/decode/filter/encode/mux/write
//
//  This is done over discrete segments. However, decode/filter/encoder are
//  expensive to re-initialize for every segment. We work around this by
//  persisting these components across segments.
//
//  The challenge with persistence is there is often internal data that is
//  buffered, and there isn't an explicit API to flush or drain that data
//  short of re-initializing the component. This is addressed for each component
//  as follows:
//
//  Demuxer: For resumable / header-less formats such as mpegts, the demuxer
//           is reused across segments. This gives a small speed boost. For
//           all other formats, the demuxer is closed and reopened at the next
//           segment.
//

// MOVED TO decoder.[ch]
//  Decoder: For audio, we pay the price of closing and re-opening the decoder.
//           For video, we cache the first packet we read (input_ctx.first_pkt).
//           The pts is set to a sentinel value and fed to the decoder. Once we
//           receive all frames from the decoder OR have sent too many sentinel
//           pkts without receiving anything, then we know the decoder has been
//           fully flushed.

// MOVED TO filter.[ch]
//  Filter:  The challenge here is around fps filter adding and dropping frames.
//           The fps filter expects a strictly monotonic input pts: frames with
//           earlier timestamps get dropped, and frames with too-late timestamps
//           will see a bunch of duplicated frames be generated to catch up with
//           the timestamp that was just inserted. So we cache the last seen
//           frame, rewrite the PTS based on the expected duration, and set a
//           sentinel field (AVFrame.opaque). Then do a lot of rewriting to
//           accommodate changes. See the notes in the filter_ctx struct and the
//           process_out function. This is done for both audio and video.
//
//           XXX No longer true update docs
//           One consequence of this behavior is that we currently cannot
//           process segments out of order, due to the monotonicity requirement.

// MOVED TO encoder.[ch]
// Encoder:  For software encoding, we close the encoder and re-open.
//           For Nvidia encoding, there is luckily an API available via
//           avcodec_flush_buffers to flush the encoder.
//

#define MAX_OUTPUT_SIZE 10

struct transcode_thread {
  int initialized;

  struct input_ctx ictx;
  struct output_ctx outputs[MAX_OUTPUT_SIZE];

  int nb_outputs;

};

void lpms_init(enum LPMSLogLevel max_level)
{
  av_log_set_level(max_level);
}

//
// Transcoder
//

static int is_mpegts(AVFormatContext *ic) {
  return !strcmp("mpegts", ic->iformat->name);
}

static int flush_outputs(struct input_ctx *ictx, struct output_ctx *octx)
{
  // only issue w this flushing method is it's not necessarily sequential
  // wrt all the outputs; might want to iterate on each output per frame?
  int ret = 0;
  if (octx->vc) { // flush video
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->vc, octx->oc->streams[octx->vi], &octx->vf, NULL);
    }
  }
  ret = 0;
  if (octx->ac) { // flush audio
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out(ictx, octx, octx->ac, octx->oc->streams[octx->ai], &octx->af, NULL);
    }
  }
  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  return av_write_trailer(octx->oc);
}

static int flush_outputs1(struct decode_meta *dmeta, struct output_ctx *octx)
{
  // only issue w this flushing method is it's not necessarily sequential
  // wrt all the outputs; might want to iterate on each output per frame?
  int ret = 0;
  if (octx->vc) { // flush video
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out1(dmeta, octx, octx->vc, octx->oc->streams[octx->vi], &octx->vf, NULL);
    }
  }
  ret = 0;
  if (octx->ac) { // flush audio
    while (!ret || ret == AVERROR(EAGAIN)) {
      ret = process_out1(dmeta, octx, octx->ac, octx->oc->streams[octx->ai], &octx->af, NULL);
    }
  }
  av_interleaved_write_frame(octx->oc, NULL); // flush muxer
  return av_write_trailer(octx->oc);
}

int transcode(struct transcode_thread *h,
  input_params *inp, output_params *params,
  output_results *results, output_results *decoded_results)
{
  int ret = 0, i = 0;
  int reopen_decoders = 1;
  struct input_ctx *ictx = &h->ictx;
  struct output_ctx *outputs = h->outputs;
  int nb_outputs = h->nb_outputs;
  AVPacket ipkt = {0};
  // AVFrame *dframe = NULL;
  dframemeta dframe[MAX_DFRAME_CNT]; 
  if (!inp) LPMS_ERR(transcode_cleanup, "Missing input params")

  // by default we re-use decoder between segments of same stream
  // unless we are using SW deocder and had to re-open IO or demuxer
  if (!ictx->ic) {
    // reopen demuxer for the input segment if needed
    // XXX could open_input() be re-used here?
    ret = avformat_open_input(&ictx->ic, inp->fname, NULL, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen demuxer");
    ret = avformat_find_stream_info(ictx->ic, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to find info for reopened stream")
  } else if (!ictx->ic->pb) {
    // reopen input segment file IO context if needed
    ret = avio_open(&ictx->ic->pb, inp->fname, AVIO_FLAG_READ);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen file");
  } else reopen_decoders = 0;
  if (reopen_decoders) {
    // XXX check to see if we can also reuse decoder for sw decoding
    if (AV_HWDEVICE_TYPE_CUDA != ictx->hw_type) {
      ret = open_video_decoder(inp, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen video decoder");
    }
    ret = open_audio_decoder(inp, ictx);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen audio decoder")
  }

  // populate output contexts
  for (i = 0; i <  nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      octx->fname = params[i].fname;
      octx->width = params[i].w;
      octx->height = params[i].h;
      octx->muxer = &params[i].muxer;
      octx->audio = &params[i].audio;
      octx->video = &params[i].video;
      octx->vfilters = params[i].vfilters;
      if (params[i].bitrate) octx->bitrate = params[i].bitrate;
      if (params[i].fps.den) octx->fps = params[i].fps;
      if (params[i].gop_time) octx->gop_time = params[i].gop_time;
      octx->dv = ictx->vi < 0 || is_drop(octx->video->name);
      octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
      octx->res = &results[i];

      // first segment of a stream, need to initalize output HW context
      // XXX valgrind this line up
      if (!h->initialized || AV_HWDEVICE_TYPE_NONE == octx->hw_type) {
        ret = open_output(octx, ictx);
        if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open output");
        continue;
      }
      
      // non-first segment of a HW session
      ret = reopen_output(octx, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to re-open output for HW session");
  }

  for(int dfcount=0; dfcount < MAX_DFRAME_CNT; dfcount++){
    dframe[dfcount].dec_frame = av_frame_alloc();
    av_init_packet(&dframe[dfcount].in_pkt);
    if (!dframe[dfcount].dec_frame) LPMS_ERR(transcode_cleanup, "Unable to allocate frame");
  }
  int dfcount = 0;
  while (1) {
    // DEMUXING & DECODING
    int has_frame = 0;
    AVStream *ist = NULL;
    AVFrame *last_frame = NULL;
    av_frame_unref(dframe[dfcount].dec_frame);

    ret = process_in(ictx, dframe[dfcount].dec_frame, &dframe[dfcount].in_pkt);
    if (ret == AVERROR_EOF) break;
                            // Bail out on streams that appear to be broken
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (ret < 0) LPMS_ERR(transcode_cleanup, "Could not decode; stopping");
    ist = ictx->ic->streams[dframe[dfcount].in_pkt.stream_index];
    has_frame = lpms_ERR_PACKET_ONLY != ret;
    dframe[dfcount].has_frame = has_frame;  //nick
    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      if (is_flush_frame(dframe[dfcount].dec_frame)) continue;
      // width / height will be zero for pure streamcopy (no decoding)
      decoded_results->frames += dframe[dfcount].dec_frame->width && dframe[dfcount].dec_frame->height;
      decoded_results->pixels += dframe[dfcount].dec_frame->width * dframe[dfcount].dec_frame->height;
      has_frame = has_frame && dframe[dfcount].dec_frame->width && dframe[dfcount].dec_frame->height;
      dframe[dfcount].has_frame = has_frame;
      if (has_frame) last_frame = ictx->last_frame_v;
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      has_frame = has_frame && dframe[dfcount].dec_frame->nb_samples;
      dframe[dfcount].has_frame = has_frame;
      if (has_frame) last_frame = ictx->last_frame_a;
    }
    if (has_frame) {
      int64_t dur = 0;
      if (dframe[dfcount].dec_frame->pkt_duration) dur = dframe[dfcount].dec_frame->pkt_duration;
      else if (ist->r_frame_rate.den) {
        dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
      } else {
        // TODO use better heuristics for this; look at how ffmpeg does it
        LPMS_WARN("Could not determine next pts; filter might drop");
      }
      dframe[dfcount].dec_frame->pkt_duration = dur;
      av_frame_unref(last_frame);
      av_frame_ref(last_frame, dframe[dfcount].dec_frame);
      dfcount++;
    }
  }
  
  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    struct filter_ctx *filter = NULL;
    AVStream *ost = NULL;
    AVStream *ist = NULL;
    AVCodecContext *encoder = NULL;
    int rewind_flag = 0;
    for(int cnt=0; cnt < dfcount; cnt++){
      ret = 0; // reset to avoid any carry-through
      ist = ictx->ic->streams[dframe[cnt].in_pkt.stream_index];
      if (ist->index == ictx->vi) {
        if (octx->dv) continue; // drop video stream for this output
                
        ost = octx->oc->streams[0];
        if (ictx->vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        }
      } else if (ist->index == ictx->ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[!octx->dv]; // depends on whether video exists
        if (ictx->ac) {
          encoder = octx->ac;
          filter = &octx->af;
        }
      } else continue; // dropped or unrecognized stream

      if (!encoder && ost) {
        // stream copy
        AVPacket *pkt;
        // we hit this case when decoder is flushing; will be no input packet
        // (we don't need decoded frames since this stream is doing a copy)
        if (dframe[cnt].in_pkt.pts == AV_NOPTS_VALUE) continue;

        pkt = av_packet_clone(&dframe[cnt].in_pkt);
        if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for copy");
        ret = mux(pkt, ist->time_base, octx, ost);
        av_packet_free(&pkt);
      } else if (dframe[cnt].has_frame) {
        ret = process_out(ictx, octx, encoder, ost, filter, dframe[cnt].dec_frame);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) LPMS_ERR(transcode_cleanup, "Error encoding");
    }
    ret = flush_outputs(ictx, &outputs[i]);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to fully flush outputs")
  }
  for(int j=0; j < dfcount; j++)
    av_packet_unref(&dframe[j].in_pkt);

transcode_cleanup:
  if (ictx->ic) {
    // Only mpegts reuse the demuxer for subsequent segments.
    // Close the demuxer for everything else.
    // TODO might be reusable with fmp4 ; check!
    if (!is_mpegts(ictx->ic)) avformat_close_input(&ictx->ic);
    else if (ictx->ic->pb) {
      // Reset leftovers from demuxer internals to prepare for next segment
      avio_flush(ictx->ic->pb);
      avformat_flush(ictx->ic);
      avio_closep(&ictx->ic->pb);
    }
  }
  for (int j=0; j < MAX_DFRAME_CNT; j++){
    if (dframe[j].dec_frame) av_frame_free(&(dframe[j].dec_frame));
  }
  ictx->flushed = 0;
  ictx->flushing = 0;
  ictx->pkt_diff = 0;
  ictx->sentinel_count = 0;
  av_packet_unref(&ipkt);  // needed for early exits
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) close_output(&outputs[i]);
  return ret == AVERROR_EOF ? 0 : ret;
}

int lpms_transcode(input_params *inp, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results)
{
  int ret = 0;
  struct transcode_thread *h = inp->handle;

  if (!h->initialized) {
    int i = 0;
    int decode_a = 0, decode_v = 0;
    if (nb_outputs > MAX_OUTPUT_SIZE) {
      return lpms_ERR_OUTPUTS;
    }

    // Check to see if we can skip decoding
    for (i = 0; i < nb_outputs; i++) {
      if (!needs_decoder(params[i].video.name)) h->ictx.dv = ++decode_v == nb_outputs;
      if (!needs_decoder(params[i].audio.name)) h->ictx.da = ++decode_a == nb_outputs;
    }

    h->nb_outputs = nb_outputs;

    // populate input context
    ret = open_input(inp, &h->ictx);
    if (ret < 0) {
      return ret;
    }
  }

  if (h->nb_outputs != nb_outputs) {
    return lpms_ERR_OUTPUTS; // Not the most accurate error...
  }

  ret = transcode(h, inp, params, results, decoded_results);
  h->initialized = 1;

  return ret;
}

struct transcode_thread* lpms_transcode_new() {
  struct transcode_thread *h = malloc(sizeof (struct transcode_thread));
  if (!h) return NULL;
  memset(h, 0, sizeof *h);
  return h;
}

void lpms_transcode_stop(struct transcode_thread *handle) {
  // not threadsafe as-is; calling function must ensure exclusivity!

  int i;

  if (!handle) return;

  free_input(&handle->ictx);
  for (i = 0; i < MAX_OUTPUT_SIZE; i++) {
    free_output(&handle->outputs[i]);
  }

  free(handle);
}


int lpms_encode(input_params *inp, dframe_buffer *dframe_buffer, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results, struct decode_meta *dmeta)
{
  printf("width %d\n", dmeta->v_width);
  int ret = 0, i = 0;
  struct transcode_thread *h = inp->handle;
  // struct transcode_thread *dec_handle = inp->dec_handle;
  h->nb_outputs = nb_outputs;
  int reopen_decoders = 1;
  struct input_ctx *ictx = &h->ictx;
  printf("drop audio enc %d %x lastframe=%x\n", ictx->da, ictx->ic, ictx->last_frame_v);
  struct output_ctx *outputs = h->outputs;
  AVPacket ipkt = {0};
  // populate output contexts
  for (i = 0; i <  nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      octx->fname = params[i].fname;
      octx->width = params[i].w;
      octx->height = params[i].h;
      octx->muxer = &params[i].muxer;
      octx->audio = &params[i].audio;
      octx->video = &params[i].video;
      octx->vfilters = params[i].vfilters;
      if (params[i].bitrate) octx->bitrate = params[i].bitrate;
      if (params[i].fps.den) octx->fps = params[i].fps;
      if (params[i].gop_time) octx->gop_time = params[i].gop_time;
      octx->dv = ictx->vi < 0 || is_drop(octx->video->name);
      // octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
      octx->da = 1;   // temporary fix to force drop audio
      octx->res = &results[i];
      // first segment of a stream, need to initalize output HW context
      // XXX valgrind this line up
      if (!h->initialized || AV_HWDEVICE_TYPE_NONE == octx->hw_type) {
        ret = open_output(octx, ictx);
        if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open output");
        continue;
      }
      // non-first segment of a HW session
      ret = reopen_output(octx, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to re-open output for HW session");
  }

  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    struct filter_ctx *filter = NULL;
    AVStream *ost = NULL;
    AVStream *ist = NULL;
    AVCodecContext *encoder = NULL;
    int rewind_flag = 0;
    for(int cnt=0; cnt < dframe_buffer->cnt; cnt++){
      ret = 0; // reset to avoid any carry-through
      ist = ictx->ic->streams[dframe_buffer->dframes[cnt].in_pkt.stream_index];
      if (ist->index == ictx->vi) {
        if (octx->dv) continue; // drop video stream for this output
                
        ost = octx->oc->streams[0];
        if (ictx->vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        }
      } else if (ist->index == ictx->ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[!octx->dv]; // depends on whether video exists
        if (ictx->ac) {
          encoder = octx->ac;
          filter = &octx->af;
        }
      } else continue; // dropped or unrecognized stream

      if (!encoder && ost) {
        // stream copy
        AVPacket *pkt;
        // we hit this case when decoder is flushing; will be no input packet
        // (we don't need decoded frames since this stream is doing a copy)
        if (dframe_buffer->dframes[cnt].in_pkt.pts == AV_NOPTS_VALUE) continue;

        pkt = av_packet_clone(&dframe_buffer->dframes[cnt].in_pkt);
        if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for copy");
        ret = mux(pkt, ist->time_base, octx, ost);
        av_packet_free(&pkt);
      } else if (dframe_buffer->dframes[cnt].has_frame) {
        ret = process_out(ictx, octx, encoder, ost, filter, dframe_buffer->dframes[cnt].dec_frame);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) LPMS_ERR(transcode_cleanup, "Error encoding");
    }
    ret = flush_outputs(ictx, &outputs[i]);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to fully flush outputs")
  }
  for(int j=0; j < dframe_buffer->cnt; j++)
    av_packet_unref(&dframe_buffer->dframes[j].in_pkt);
  // if(dframe_buffer->dframes != NULL)
  //   free(dframe_buffer->dframes);

transcode_cleanup:
  // if (ictx->ic) {
  //   // Only mpegts reuse the demuxer for subsequent segments.
  //   // Close the demuxer for everything else.
  //   // TODO might be reusable with fmp4 ; check!
  //   if (!is_mpegts(ictx->ic)) avformat_close_input(&ictx->ic);
  //   else if (ictx->ic->pb) {
  //     // Reset leftovers from demuxer internals to prepare for next segment
  //     avio_flush(ictx->ic->pb);
  //     avformat_flush(ictx->ic);
  //     avio_closep(&ictx->ic->pb);
  //   }
  // }
  for (int j=0; j < MAX_DFRAME_CNT; j++){
    if (dframe_buffer->dframes[j].dec_frame) av_frame_free(&(dframe_buffer->dframes[j].dec_frame));
  }
  // ictx->flushed = 0;
  // ictx->flushing = 0;
  // ictx->pkt_diff = 0;
  // ictx->sentinel_count = 0;
  // av_packet_unref(&ipkt);  // needed for early exits
  // if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  // if (ictx->ac) avcodec_free_context(&ictx->ac);
  // if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) close_output(&outputs[i]);
  h->initialized = 1;
  return ret == AVERROR_EOF ? 0 : ret;
}


int lpms_encode1(input_params *inp, dframe_buffer *dframe_buffer, output_params *params,
  output_results *results, int nb_outputs, output_results *decoded_results, struct decode_meta *dmeta)
{
  int ret = 0, i = 0;
  struct transcode_thread *h = inp->handle;
  // struct transcode_thread *dec_handle = inp->dec_handle;
  h->nb_outputs = nb_outputs;
  int reopen_decoders = 1;
  // struct input_ctx *ictx = &h->ictx;
  // printf("drop audio enc %d %x lastframe=%x\n", ictx->da, ictx->ic, ictx->last_frame_v);
  struct output_ctx *outputs = h->outputs;
  AVPacket ipkt = {0};
  // populate output contexts
  for (i = 0; i <  nb_outputs; i++) {
      struct output_ctx *octx = &outputs[i];
      octx->fname = params[i].fname;
      octx->width = params[i].w;
      octx->height = params[i].h;
      octx->muxer = &params[i].muxer;
      octx->audio = &params[i].audio;
      octx->video = &params[i].video;
      octx->vfilters = params[i].vfilters;
      if (params[i].bitrate) octx->bitrate = params[i].bitrate;
      if (params[i].fps.den) octx->fps = params[i].fps;
      if (params[i].gop_time) octx->gop_time = params[i].gop_time;
      octx->dv = dmeta->vi < 0 || is_drop(octx->video->name);
      // octx->da = ictx->ai < 0 || is_drop(octx->audio->name);
      octx->da = 1;   // temporary fix to force drop audio
      octx->res = &results[i];
      // first segment of a stream, need to initalize output HW context
      // XXX valgrind this line up
      if (!h->initialized || AV_HWDEVICE_TYPE_NONE == octx->hw_type) {
        ret = open_output1(octx, dmeta);
        if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to open output");
        continue;
      }
      // non-first segment of a HW session
      ret = reopen_output1(octx, dmeta);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to re-open output for HW session");
  }

  for (i = 0; i < nb_outputs; i++) {
    struct output_ctx *octx = &outputs[i];
    struct filter_ctx *filter = NULL;
    AVStream *ost = NULL;
    AVStream *ist = NULL;
    AVCodecContext *encoder = NULL;
    int rewind_flag = 0;
    for(int cnt=0; cnt < dframe_buffer->cnt; cnt++){
      ret = 0; // reset to avoid any carry-through
      // ist = ictx->ic->streams[dframe_buffer->dframes[cnt].in_pkt.stream_index];
      int stream_index;
      stream_index = dframe_buffer->dframes[cnt].in_pkt.stream_index;
      if (stream_index == dmeta->vi) {
        if (octx->dv) continue; // drop video stream for this output
                
        ost = octx->oc->streams[0];
        // if (ictx->vc) {
          encoder = octx->vc;
          filter = &octx->vf;
        // }
      } else if (stream_index == dmeta->ai) {
        if (octx->da) continue; // drop audio stream for this output
        ost = octx->oc->streams[!octx->dv]; // depends on whether video exists
        // if (ictx->ac) {
          encoder = octx->ac;
          filter = &octx->af;
        // }
      } else continue; // dropped or unrecognized stream

      if (!encoder && ost) {
        // stream copy
        AVPacket *pkt;
        // we hit this case when decoder is flushing; will be no input packet
        // (we don't need decoded frames since this stream is doing a copy)
        if (dframe_buffer->dframes[cnt].in_pkt.pts == AV_NOPTS_VALUE) continue;

        pkt = av_packet_clone(&dframe_buffer->dframes[cnt].in_pkt);
        if (!pkt) LPMS_ERR(transcode_cleanup, "Error allocating packet for copy");
        ret = mux(pkt, ist->time_base, octx, ost);
        av_packet_free(&pkt);
      } else if (dframe_buffer->dframes[cnt].has_frame) {
        ret = process_out1(dmeta, octx, encoder, ost, filter, dframe_buffer->dframes[cnt].dec_frame);
      }
      if (AVERROR(EAGAIN) == ret || AVERROR_EOF == ret) continue;
      else if (ret < 0) LPMS_ERR(transcode_cleanup, "Error encoding");
    }
    ret = flush_outputs1(dmeta, &outputs[i]);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to fully flush outputs")
  }
  for(int j=0; j < dframe_buffer->cnt; j++)
    av_packet_unref(&dframe_buffer->dframes[j].in_pkt);
  // if(dframe_buffer->dframes != NULL)
  //   free(dframe_buffer->dframes);

transcode_cleanup:
  // if (ictx->ic) {
  //   // Only mpegts reuse the demuxer for subsequent segments.
  //   // Close the demuxer for everything else.
  //   // TODO might be reusable with fmp4 ; check!
  //   if (!is_mpegts(ictx->ic)) avformat_close_input(&ictx->ic);
  //   else if (ictx->ic->pb) {
  //     // Reset leftovers from demuxer internals to prepare for next segment
  //     avio_flush(ictx->ic->pb);
  //     avformat_flush(ictx->ic);
  //     avio_closep(&ictx->ic->pb);
  //   }
  // }
  for (int j=0; j < MAX_DFRAME_CNT; j++){
    if (dframe_buffer->dframes[j].dec_frame) av_frame_free(&(dframe_buffer->dframes[j].dec_frame));
  }
  // ictx->flushed = 0;
  // ictx->flushing = 0;
  // ictx->pkt_diff = 0;
  // ictx->sentinel_count = 0;
  // av_packet_unref(&ipkt);  // needed for early exits
  // if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  // if (ictx->ac) avcodec_free_context(&ictx->ac);
  // if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  for (i = 0; i < nb_outputs; i++) close_output(&outputs[i]);
  h->initialized = 1;
  return ret == AVERROR_EOF ? 0 : ret;
}

void copy_ictx(struct input_ctx *dest, struct input_ctx *src){
  dest->vi = src->vi;
  dest->ai = src->ai;
  dest->dv = src->dv;
  dest->da = src->da;
  dest->flushed = src->flushed;
  dest->flushing = src->flushing;
  dest->pkt_diff = src->pkt_diff;
  dest->hw_type = src->hw_type;
  dest->sentinel_count = src->sentinel_count;

  dest->vc = src->vc;
  dest->ac = src->ac;
  // dest->ic = avformat_alloc_context();
  
  dest->ic = src->ic;

  // dest->hw_device_ctx = src->hw_device_ctx;
  dest->device = src->device;
  dest->first_pkt = src->first_pkt;
  dest->last_frame_v = src->last_frame_v;
  // dest->last_frame_a = src->last_frame_a;
  // if(src->last_frame_a)
  // {
  //   dest->last_frame_a = av_frame_alloc();
  //   deep_copy_avframe(dest->last_frame_a, src->last_frame_a);
  // }

  // if(src->last_frame_v)
  // {
  //   dest->last_frame_v = av_frame_alloc();
  //   deep_copy_avframe(dest->last_frame_v, src->last_frame_v);
  // }
}
int deep_copy_avframe(AVFrame *dest, AVFrame *src){
  int ret;
  // memcpy(dest,src,sizeof(AVFrame));
  dest->format = src->format;
  dest->width = src->width;
  dest->height = src->height;
  dest->channels = src->channels;
  dest->channel_layout = src->channel_layout;
  dest->nb_samples = src->nb_samples;
  ret = av_frame_get_buffer(dest, 32);
  if (ret < 0)
    return ret;

  ret = av_frame_copy(dest, src);
  if (ret < 0) {
      av_frame_unref(dest);
      return ret;
  }
  av_frame_copy_props(dest, src);
  av_frame_unref(dest);
  dest->extended_data  = src->extended_data;
  return 0;
};

void set_ictx(struct transcode_thread *h, struct input_ctx *ictx){
  copy_ictx(&h->ictx, ictx);
};

void set_dmeta(struct decode_meta *dmeta, struct input_ctx *ictx){
  dmeta->v_width = ictx->vc->width;
  dmeta->v_height = ictx->vc->height;
  dmeta->vi = ictx->vi;
  dmeta->ai = ictx->ai;
  dmeta->in_pix_fmt = ictx->vc->pix_fmt;
  dmeta->time_base = ictx->ic->streams[ictx->vi]->time_base;
  dmeta->sample_aspect_ratio = ictx->vc->sample_aspect_ratio;
  dmeta->r_frame_rate = ictx->ic->streams[ictx->vi]->r_frame_rate;
  dmeta->framerate = ictx->vc->framerate;
  dmeta->hw_type = ictx->hw_type;
  dmeta->hw_frames_ctx = ictx->vc->hw_frames_ctx;
  if (!dmeta->last_frame_v)
    dmeta->last_frame_v = av_frame_alloc();
  av_frame_unref(dmeta->last_frame_v);
      // av_frame_ref(last_frame, dframe[dfcount].dec_frame);
  av_frame_ref(dmeta->last_frame_v, ictx->last_frame_v);
};

int decode(struct transcode_thread *h,
  input_params *inp, output_results *decoded_results, dframe_buffer *dframe_buf, struct input_ctx *ictx_temp, struct decode_meta *dmeta)
{
  int ret = 0, i = 0;
  int reopen_decoders = 1;
  struct input_ctx *ictx = &h->ictx;
  ictx->da = 1; //temporary fix to drop audio
  AVPacket ipkt = {0};
  dframe_buf->cnt = 0;
  dframe_buf->dframes =  malloc(sizeof(dframemeta) * MAX_DFRAME_CNT);
  
  if (!inp) LPMS_ERR(transcode_cleanup, "Missing input params")

  // by default we re-use decoder between segments of same stream
  // unless we are using SW deocder and had to re-open IO or demuxer
  if (!ictx->ic) {
    // reopen demuxer for the input segment if needed
    // XXX could open_input() be re-used here?
    ret = avformat_open_input(&ictx->ic, inp->fname, NULL, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen demuxer");
    ret = avformat_find_stream_info(ictx->ic, NULL);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to find info for reopened stream")
  } else if (!ictx->ic->pb) {
    // reopen input segment file IO context if needed
    ret = avio_open(&ictx->ic->pb, inp->fname, AVIO_FLAG_READ);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen file");
  } else reopen_decoders = 0;
  if (reopen_decoders) {
    // XXX check to see if we can also reuse decoder for sw decoding
    if (AV_HWDEVICE_TYPE_CUDA != ictx->hw_type) {
      ret = open_video_decoder(inp, ictx);
      if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen video decoder");
    }
    ret = open_audio_decoder(inp, ictx);
    if (ret < 0) LPMS_ERR(transcode_cleanup, "Unable to reopen audio decoder")
  }

  for(int dfcount=0; dfcount < MAX_DFRAME_CNT; dfcount++){
    // dframe[dfcount].dec_frame = av_frame_alloc();
    // av_init_packet(&dframe[dfcount].in_pkt);
    // if (!dframe[dfcount].dec_frame) LPMS_ERR(transcode_cleanup, "Unable to allocate frame");
    dframe_buf->dframes[dfcount].dec_frame = av_frame_alloc();
    av_init_packet(&(dframe_buf->dframes[dfcount].in_pkt));
    if (!dframe_buf->dframes[dfcount].dec_frame) LPMS_ERR(transcode_cleanup, "Unable to allocate frame");
  }
  int dfcount = 0;
  while (1) {
    // DEMUXING & DECODING
    int has_frame = 0;
    AVStream *ist = NULL;
    AVFrame *last_frame = NULL;
    // av_frame_unref(dframe[dfcount].dec_frame);
    av_frame_unref(dframe_buf->dframes[dfcount].dec_frame);
    // ret = process_in(ictx, dframe[dfcount].dec_frame, &dframe[dfcount].in_pkt);
    ret = process_in(ictx, dframe_buf->dframes[dfcount].dec_frame, &dframe_buf->dframes[dfcount].in_pkt);
    if (ret == AVERROR_EOF) break;
                            // Bail out on streams that appear to be broken
    else if (lpms_ERR_PACKET_ONLY == ret) ; // keep going for stream copy
    else if (ret < 0) LPMS_ERR(transcode_cleanup, "Could not decode; stopping");
    // ist = ictx->ic->streams[dframe[dfcount].in_pkt.stream_index];
    ist = ictx->ic->streams[dframe_buf->dframes[dfcount].in_pkt.stream_index];

    has_frame = lpms_ERR_PACKET_ONLY != ret;
    // dframe[dfcount].has_frame = has_frame;  //nick
    dframe_buf->dframes[dfcount].has_frame = has_frame;  
    if (AVMEDIA_TYPE_VIDEO == ist->codecpar->codec_type) {
      // if (is_flush_frame(dframe[dfcount].dec_frame)) continue;
      if (is_flush_frame(dframe_buf->dframes[dfcount].dec_frame)) continue;
      // width / height will be zero for pure streamcopy (no decoding)
      // decoded_results->frames += dframe[dfcount].dec_frame->width && dframe[dfcount].dec_frame->height;
      // decoded_results->pixels += dframe[dfcount].dec_frame->width * dframe[dfcount].dec_frame->height;
      // has_frame = has_frame && dframe[dfcount].dec_frame->width && dframe[dfcount].dec_frame->height;
      // dframe[dfcount].has_frame = has_frame;
      decoded_results->frames += dframe_buf->dframes[dfcount].dec_frame->width && dframe_buf->dframes[dfcount].dec_frame->height;
      decoded_results->pixels += dframe_buf->dframes[dfcount].dec_frame->width * dframe_buf->dframes[dfcount].dec_frame->height;
      has_frame = has_frame && dframe_buf->dframes[dfcount].dec_frame->width && dframe_buf->dframes[dfcount].dec_frame->height;
      dframe_buf->dframes[dfcount].has_frame = has_frame;
      if (has_frame) last_frame = ictx->last_frame_v;
    } else if (AVMEDIA_TYPE_AUDIO == ist->codecpar->codec_type) {
      // has_frame = has_frame && dframe[dfcount].dec_frame->nb_samples;
      // dframe[dfcount].has_frame = has_frame;
      has_frame = has_frame && dframe_buf->dframes[dfcount].dec_frame->nb_samples;
      dframe_buf->dframes[dfcount].has_frame = has_frame;
      if (has_frame) last_frame = ictx->last_frame_a;
    }
    if (has_frame) {
      int64_t dur = 0;
      // if (dframe[dfcount].dec_frame->pkt_duration) dur = dframe[dfcount].dec_frame->pkt_duration;
      if (dframe_buf->dframes[dfcount].dec_frame->pkt_duration) dur = dframe_buf->dframes[dfcount].dec_frame->pkt_duration;
      else if (ist->r_frame_rate.den) {
        dur = av_rescale_q(1, av_inv_q(ist->r_frame_rate), ist->time_base);
      } else {
        // TODO use better heuristics for this; look at how ffmpeg does it
        LPMS_WARN("Could not determine next pts; filter might drop");
      }
      // dframe[dfcount].dec_frame->pkt_duration = dur;
      dframe_buf->dframes[dfcount].dec_frame->pkt_duration = dur;
      // printf("lastframe %x %x decframe %d %x lastframe=%x\n", ictx->last_frame_a, ictx->last_frame_v, dfcount, dframe_buf->dframes[dfcount].dec_frame, last_frame);
      av_frame_unref(last_frame);
      // av_frame_ref(last_frame, dframe[dfcount].dec_frame);
      av_frame_ref(last_frame, dframe_buf->dframes[dfcount].dec_frame);
         
      dfcount++;
    }
  }
  dframe_buf->cnt = dfcount;
  	
transcode_cleanup:
  if (ictx->ic) {
    // Only mpegts reuse the demuxer for subsequent segments.
    // Close the demuxer for everything else.
    // TODO might be reusable with fmp4 ; check!
    if (!is_mpegts(ictx->ic)) avformat_close_input(&ictx->ic);
    else if (ictx->ic->pb) {
      // Reset leftovers from demuxer internals to prepare for next segment
      avio_flush(ictx->ic->pb);
      avformat_flush(ictx->ic);
      avio_closep(&ictx->ic->pb);
    }
  }
  ictx->flushed = 0;
  ictx->flushing = 0;
  ictx->pkt_diff = 0;
  ictx->sentinel_count = 0;
  av_packet_unref(&ipkt);  // needed for early exits
  if (ictx->first_pkt) av_packet_free(&ictx->first_pkt);
  if (ictx->ac) avcodec_free_context(&ictx->ac);
  if (ictx->vc && AV_HWDEVICE_TYPE_NONE == ictx->hw_type) avcodec_free_context(&ictx->vc);
  // copy_ictx(ictx_temp, ictx);
  set_dmeta(dmeta, ictx);
  return ret == AVERROR_EOF ? 0 : ret;
}

int lpms_decode(input_params *inp,  output_results *decoded_results, dframe_buffer *dframe_buf, struct input_ctx *ictx, struct decode_meta *dmeta)
{
  int ret = 0;
  struct transcode_thread *h = inp->dec_handle;
  if (!h->initialized) {
    int i = 0;
    int decode_a = 0, decode_v = 0;

    // populate input context
    ret = open_input(inp, &h->ictx);
    // ret = open_input(inp, ictx);
    if (ret < 0) {
      return ret;
    }
  }
  ret = decode(h, inp, decoded_results, dframe_buf, ictx, dmeta);
  h->initialized = 1;

  return ret;
}

struct decode_meta* alloc_decode_meta(){
  struct decode_meta *dmeta = malloc(sizeof (struct decode_meta));
  if (!dmeta) return NULL;
  memset(dmeta, 0, sizeof *dmeta);
  return dmeta; 
}