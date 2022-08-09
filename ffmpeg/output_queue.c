#include "output_queue.h"

void queue_create(OutputQueue *queue)
{
  pthread_mutex_init(&queue->mutex, NULL);
  pthread_cond_init(&queue->condition, NULL);
  queue->front = queue->back = NULL;
}

void queue_destroy(OutputQueue *queue)
{
  pthread_mutex_destroy(&queue->mutex);
  pthread_cond_destroy(&queue->condition);
  queue_reset(queue);
}

static int queue_write_function(void *user_data, uint8_t *buf, int buf_size)
{
  WriteContext *wctx = (WriteContext *)user_data;
  // Prepare packet
  OutputPacket *packet = (OutputPacket *)malloc(sizeof(OutputPacket));
  if (!packet) return -1;
  packet->data = (uint8_t *)malloc(buf_size);
  if (!packet->data) {
    free(packet);
    return -1;
  }
  memcpy(packet->data, buf, buf_size);
  packet->size = buf_size;
  packet->index = wctx->index;
  packet->next = NULL;
  // Important - we are not adding to the queue now. This is because we don't
  // know which flags to assign yet - for example, we can't assign END_OF_OUTPUT
  // because we don't know if we are at last packet or not. Instead, packets are
  // added to the staging area and queue_push_staging will be used to add them
  // to the queue after muxing operation finishes.
  if (wctx->staging_back) {
    // not the first packet
    wctx->staging_back->next = packet;
    wctx->staging_back = packet;
  } else {
    // first packet
    wctx->staging_front = wctx->staging_back = packet;
  }
  return buf_size;
}

int queue_setup_as_output(OutputQueue *queue, WriteContext *wctx, AVFormatContext *ctx)
{
  wctx->queue = queue;
  wctx->staging_front = wctx->staging_back = NULL;
  // IMPORTANT: I am not sure if ffmpeg documentation states that explicitly,
  // but the memory of ctx->pb as well as its io_buffer seem to be released when
  // ctx will get closed. I tried otherwise and got "double free" errors
#define BUFFER_SIZE 4096
  void *io_buffer = av_malloc(BUFFER_SIZE);
  if (!io_buffer) return -1;
  ctx->pb = avio_alloc_context(
    io_buffer, BUFFER_SIZE,  // buffer and size
    1,                       // write allowed
    wctx,                    // pass write context as user data
    NULL,                    // no read function supplied
    queue_write_function,
    NULL);                   // no seek function supplied
  if (!ctx->pb) return -1;
  ctx->flags |= AVFMT_FLAG_CUSTOM_IO | AVFMT_FLAG_FLUSH_PACKETS;
  return 0;
}

void queue_reset(OutputQueue *queue)
{
  while (queue->front) {
    OutputPacket *tmp = queue->front;
    queue->front = queue->front->next;
    if (tmp->data) free(tmp->data);
    free(tmp);
  }
  queue->back = NULL;
}

const OutputPacket *queue_peek_front(OutputQueue *queue)
{
  OutputPacket *tmp;
  pthread_mutex_lock(&queue->mutex);
  while (!queue->front) {
    // wait until there is packet in the buffer
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
  tmp = queue->front;
  pthread_mutex_unlock(&queue->mutex);
  return tmp;
}

void queue_pop_front(OutputQueue *queue)
{
  OutputPacket *tmp;
  pthread_mutex_lock(&queue->mutex);
  while (!queue->front) {
    // wait until there is packet in the buffer
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
  tmp = queue->front;
  queue->front = queue->front->next;
  if (!queue->front) queue->back = NULL;
  pthread_mutex_unlock(&queue->mutex);
  if (tmp->data) free(tmp->data);
  free(tmp);
}

void queue_push_staging(WriteContext *wctx, PacketFlags flags, int64_t timestamp)
{
  // iterate over staging area setting flags and timestamps
  OutputPacket *packet = wctx->staging_front;
  // Make sure that END_OF_OUTPUT only gets assigned to the last packet
  // this is because the caller knows all packets are emitted, but it
  // doesn't know how many of them
  PacketFlags safe_flags = flags & ~END_OF_OUTPUT;
  if (!packet) return;  // nothing to do
  while (packet) {
    packet->flags = packet->next ? safe_flags : flags;
    packet->timestamp = timestamp;
    packet = packet->next;
  }
  // move staging area into queue
  pthread_mutex_lock(&wctx->queue->mutex);
  if (wctx->queue->back) {
    // not empty queue
    wctx->queue->back->next = wctx->staging_front;
    wctx->queue->back = wctx->staging_back;
  } else {
    // empty queue
    wctx->queue->front = wctx->staging_front;
    wctx->queue->back = wctx->staging_back;
  }
  wctx->staging_front = wctx->staging_back = NULL;
  pthread_mutex_unlock(&wctx->queue->mutex);
  pthread_cond_signal(&wctx->queue->condition);
}

int queue_push_end(OutputQueue *queue)
{
  OutputPacket *packet = (OutputPacket *)malloc(sizeof(OutputPacket));
  if (!packet) return -1;
  packet->size = 0;
  packet->data = NULL;
  packet->timestamp = -1;
  packet->flags = END_OF_ALL_OUTPUTS;
  pthread_mutex_lock(&queue->mutex);
  if (queue->back) {
    queue->back->next = packet;
    queue->back = packet;
  } else {
    queue->front = queue->back = packet;
  }
  pthread_mutex_unlock(&queue->mutex);
  pthread_cond_signal(&queue->condition);
  return 0;
}
