#include "queue.h"

/**
 * Queue for buffering frames and packets while the hardware video decoder initializes
 */

/**
 * Each queue item holds both an AVPacket* and an AVFrame*.
 */
typedef struct {
    AVPacket *pkt;
    AVFrame  *frame;
    int decoder_return;
} queue_item;

AVFifo* queue_create()
{
  // Create a FIFO that can hold 8 items initially, each of size queue_item,
  // and auto-grow as needed.
  return av_fifo_alloc2(8, sizeof(queue_item), AV_FIFO_FLAG_AUTO_GROW);
}

void queue_free(AVFifo **fifo)
{
  if (!fifo || !*fifo) return;

  // Drain everything still in the FIFO
  queue_item item;
  memset(&item, 0, sizeof(item));
  while (av_fifo_read(*fifo, &item, 1) >= 0) {
    if (item.pkt) av_packet_free(&item.pkt);
    if (item.frame) av_frame_free(&item.frame);
  }

  av_fifo_freep2(fifo);  // Frees the buffer & sets *fifo = NULL
}

int queue_write(AVFifo *fifo, const AVPacket *pkt, const AVFrame *frame, int decoder_return)
{
  if (!fifo) return AVERROR(EINVAL);

  queue_item item;
  memset(&item, 0, sizeof(item));

  item.decoder_return = decoder_return;

  // Create a new packet reference if needed
  if (pkt) {
    item.pkt = av_packet_clone(pkt);
    if (!item.pkt) return AVERROR(EINVAL);
  }

  // Create a new frame reference if needed
  if (frame) {
    item.frame = av_frame_clone(frame);
    if (!item.frame) {
      av_packet_free(&item.pkt);
      return AVERROR(EINVAL);
    }
  }

  return av_fifo_write(fifo, &item, 1);
}

int queue_read(AVFifo *fifo, AVFrame *out_frame, AVPacket *out_pkt, int *stream_index, int *decoder_return)
{
  if (!fifo) return AVERROR(EINVAL);

  queue_item item;
  int ret = av_fifo_read(fifo, &item, 1);
  if (ret < 0) return ret;

  // Transfer ownership
  if (out_pkt && item.pkt) {
    *stream_index = item.pkt->stream_index;
    av_packet_move_ref(out_pkt, item.pkt);
  }
  av_packet_free(&item.pkt);

  if (out_frame && item.frame) {
    av_frame_move_ref(out_frame, item.frame);
  }
  av_frame_free(&item.frame);

  *decoder_return = item.decoder_return;

  return 0;
}
