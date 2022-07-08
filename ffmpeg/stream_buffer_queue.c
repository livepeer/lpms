#include <stdlib.h>
#include "stream_buffer_queue.h"

void queue_create(StreamBufferQueue *queue)
{
  pthread_mutex_init(&queue->mutex, NULL);
  pthread_cond_init(&queue->condition, NULL);
  // this means "empty queue"
  queue->front = queue->back = NULL;
}

void queue_destroy(StreamBufferQueue *queue)
{
  queue_remove_all(queue);
  pthread_mutex_destroy(&queue->mutex);
  pthread_cond_destroy(&queue->condition);
}

void queue_setup_read_context(StreamBufferQueue *queue, ReadContext *rctx)
{
  rctx->queue = queue;
  rctx->offset = 0;
  rctx->current = NULL;
}

static int queue_read_function(void *data, uint8_t *buf, int buf_size)
{
  ReadContext *rctx = (ReadContext *)data;
  if (!rctx->current) {
    // at the very begin of input
    rctx->current = queue_peek_front(rctx->queue);
  }

  int copied = 0;
  int remaining = buf_size;
  while (rctx->current && remaining) {
    // number of bytes that are available in the current buffer
    int available = rctx->current->size - rctx->offset;
    // number of bytes that can be copied
    int to_copy = available < remaining ? available : remaining;
    // copy
    memcpy(buf + copied, rctx->current->data + rctx->offset, to_copy);
    // update pointers and counters
    rctx->offset += to_copy;
    remaining -= to_copy;
    copied += to_copy;
    // move to the next element of the queue, if necessary
    if (rctx->offset == rctx->current->size) {
      // note that this call will produce NULL at the end of input
      rctx->current = queue_next(rctx->queue, rctx->current);
      rctx->offset = 0;
    }
  }

  return copied;
}

static int64_t queue_seek_function(void *data, int64_t pos, int whence)
{
  ReadContext *rctx = (ReadContext *)data;
  if (!pos && (SEEK_SET == whence)) {
    // we only support "reset to the begin"
    rctx->offset = 0;
    rctx->current = NULL;
    return 0;
  }
  return -1;
}

// TODO: pass not queue here but sth like queue reading context?
void queue_setup_as_input(AVFormatContext *ctx, ReadContext *rctx)
{
  // reset read context - we want to start at the begin of the queue
  rctx->offset = 0;
  rctx->current = NULL;
#define BUFFER_SIZE 4096
  ctx->pb = avio_alloc_context(
    av_malloc(BUFFER_SIZE), BUFFER_SIZE,  // buffer and size
    0,                                    // do not write, just read
    rctx,                                 // pass read context as user data
    queue_read_function,
    NULL,                                 // no write function supplied
    queue_seek_function);
  ctx->flags |= AVFMT_FLAG_CUSTOM_IO;
}

void queue_push_back(StreamBufferQueue *queue, StreamBuffer *buffer)
{
  // add new element first...
  pthread_mutex_lock(&queue->mutex);
  if (queue->front) {
    // adding first element to empty queue
    queue->front = queue->back = buffer;
  } else {
    // adding element at the back
    queue->back->next = buffer;
    queue->back = buffer;
  }
  buffer->next = NULL;
  pthread_mutex_unlock(&queue->mutex);
  // ...then signal condition variable
  pthread_cond_signal(&queue->condition);
}

const StreamBuffer *queue_peek_front(StreamBufferQueue *queue)
{
  const StreamBuffer *front;
  pthread_mutex_lock(&queue->mutex);
  // wait for queue not to be empty
  while (!queue->front) {
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
  front = queue->front;
  pthread_mutex_unlock(&queue->mutex);
  return front;
}

void queue_pop_front(StreamBufferQueue *queue)
{
  StreamBuffer *element;
  pthread_mutex_lock(&queue->mutex);
  // wait for queue not to be empty
  while (!queue->front) {
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
  // take away front element
  element = queue->front;
  queue->front = element->next;
  pthread_mutex_unlock(&queue->mutex);
  // release old element
  free(element->data);
  free(element);
}

void queue_remove_all(StreamBufferQueue *queue)
{
  pthread_mutex_lock(&queue->mutex);
  // remove all remaining elements
  while (queue->front) {
    StreamBuffer *tmp = queue->front;
    queue->front = queue->front->next;
    free(tmp->data);
    free(tmp);
  }
  pthread_mutex_unlock(&queue->mutex);
}

const StreamBuffer *queue_next(StreamBufferQueue *queue,
                               const StreamBuffer *current)
{
  if (END_OF_QUEUE & current->flags) {
    // end of the queue reached, no more "next" pointer
    return NULL;
  }

  pthread_mutex_lock(&queue->mutex);
  while (!current->next) {
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
  pthread_mutex_unlock(&queue->mutex);
  return current->next;
}
