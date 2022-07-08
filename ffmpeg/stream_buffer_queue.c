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
  rctx->position = 0;
}

static int ffmpeg_error(StreamErrors errors)
{
  switch (errors) {
    case NO_ENTRY: return AVERROR(ENOENT);
    default: return AVERROR(EIO);
  }
}

static int queue_read_function(void *data, uint8_t *buf, int buf_size)
{
  ReadContext *rctx = (ReadContext *)data;
  // See if we have current element
  if (!rctx->current) {
    // Nope, so there are two possibilities, depending on the stream position
    // we might be
    if (rctx->position) {
      // at the very end of input, signal EOF
      return AVERROR_EOF;
    } else {
      // or at the very begin of input, start reading
      rctx->current = queue_peek_front(rctx->queue);
    }
  }

  int copied = 0;
  int remaining = buf_size;
  while (rctx->current && remaining) {
    if (STREAM_ERROR & rctx->current->flags) {
      // error flag is set for this element, return error to emulate real i/o
      // behaviour
      return ffmpeg_error(rctx->current->error);
    }
    // number of bytes that are available in the current buffer
    int available = rctx->current->size - rctx->offset;
    // number of bytes that can be copied
    int to_copy = available < remaining ? available : remaining;
    // copy
    memcpy(buf + copied, rctx->current->data + rctx->offset, to_copy);
    // update pointers and counters
    rctx->offset += to_copy;
    rctx->position += to_copy;
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
  // FFmpeg ORs in some extra flags, so have to use AND
  if (AVSEEK_SIZE & whence) {
    // not supported, because we cannot be sure how many bytes will arrive over
    // queue
    return -1;
  }
  // remove force flag
  whence &= ~AVSEEK_FORCE;
  if ((SEEK_END == whence) && !rctx->current && rctx->position && (pos < 0)) {
    // This is a hack, and it only works when position is at the end of file,
    // but it fixes TestTranscoder_ShortSegments
    // TODO: could perhaps use better implementation for SEEK_END?
    // better idea would be to save rctx->position once end is reached, and
    // also if it wasn't reached yet we could read in remainder of a queue,
    // possibly blocking?
    whence = SEEK_SET;
    pos = rctx->position + pos;
  }
  if (SEEK_SET == whence) {
    if (pos < rctx->position) {
      // this is rewind, currently queue is single-linked list and so we
      // have to rewind to the begin
      rctx->current = queue_peek_front(rctx->queue);
      rctx->position = 0;
      rctx->offset = 0;
    } else {
      // this is move forward, see if we can do it (not already at the EOF)
      if (rctx->current) {
        // reset to the begin of current element to make code below simpler
        rctx->position -= rctx->offset;
        rctx->offset = 0;
      } else {
        // already at end of stream
        return AVERROR_EOF;
      }
    }
    // advance towards desired position
    while (rctx->position < pos) {
      int64_t after = rctx->position + rctx->current->size;
      if (after < pos) {
        // advance to next element
        const StreamBuffer *next = queue_next(rctx->queue, rctx->current);
        rctx->position = after;
        rctx->current = next;
        if (!next) return AVERROR_EOF; // end of stream while seeking
      } else {
        // move within this element
        rctx->offset = pos - rctx->position;
        rctx->position = pos;
      }
    }
    return pos;
  }
  if (!pos && (SEEK_CUR == whence)) {
    // this is "tell me where I am"
    return rctx->position;
  }
  // This handles all the other cases, -1 means "not supported" here
  return -1;
}

// TODO: pass not queue here but sth like queue reading context?
int queue_setup_as_input(AVFormatContext *ctx, ReadContext *rctx)
{
  // reset read context - we want to start at the begin of the queue
  rctx->offset = 0;
  rctx->current = NULL;
  rctx->position = 0;

  // IMPORTANT: I am not sure if ffmpeg documentation states that explicitly,
  // but the memory of ctx->pb as well as its io_buffer seem to be released when
  // ctx will get closed. I tried otherwise and got "double free" errors
#define BUFFER_SIZE 4096
  void *io_buffer = av_malloc(BUFFER_SIZE);
  if (!io_buffer) return -1;
  ctx->pb = avio_alloc_context(
    io_buffer, BUFFER_SIZE,  // buffer and size
    0,                       // do not write, just read
    rctx,                    // pass read context as user data
    queue_read_function,
    NULL,                    // no write function supplied
    queue_seek_function);
  if (!ctx->pb) return -1;
  ctx->flags |= AVFMT_FLAG_CUSTOM_IO;
  return 0;
}

void queue_push_back(StreamBufferQueue *queue, StreamBuffer *buffer)
{
  // add new element first...
  pthread_mutex_lock(&queue->mutex);
  if (!queue->front) {
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
  if (queue->back == element) queue->back = NULL; // last element removed
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
  queue->back = NULL;
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
