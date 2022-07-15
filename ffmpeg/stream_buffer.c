#include "stream_buffer.h"

int buffer_create(StreamBuffer *buffer)
{
  buffer->data = (uint8_t *)malloc(STREAM_BUFFER_BYTES);
  if (!buffer->data) return -1;
  pthread_mutex_init(&buffer->mutex, NULL);
  pthread_cond_init(&buffer->condition_put, NULL);
  pthread_cond_init(&buffer->condition_get, NULL);
  buffer_reset(buffer);
  return 0;
}

void buffer_destroy(StreamBuffer *buffer)
{
  if (buffer->data) free(buffer->data);
  pthread_mutex_destroy(&buffer->mutex);
  pthread_cond_destroy(&buffer->condition_put);
  pthread_cond_destroy(&buffer->condition_get);
  buffer->data = NULL;
  buffer->flags = 0;
}

static int64_t remaining(StreamBuffer *buffer)
{
  return STREAM_BUFFER_BYTES - buffer->unread_bytes - PROTECTED_BYTES;
}

static int ffmpeg_error(StreamErrors errors)
{
  switch (errors) {
    case NO_ENTRY: return AVERROR(ENOENT);
    default: return AVERROR(EIO);
  }
}

static int buffer_read_function(void *user_data, uint8_t *buf, int buf_size)
{
  StreamBuffer *buffer = (StreamBuffer *)user_data;
  int64_t to_read, end_offset, trailing, first_copy, second_copy;
  int ret = 0;
  pthread_mutex_lock(&buffer->mutex);
  if (buffer->flags & STREAM_ERROR) {
    // there was an error, emulate FFmpeg behavior
    ret = ffmpeg_error(buffer->error);
    goto read_error;
  }
  // wait for end of stream or some unread data
  while (!(END_OF_STREAM & buffer->flags) && !buffer->unread_bytes) {
    pthread_cond_wait(&buffer->condition_put, &buffer->mutex);
  }
  if ((END_OF_STREAM & buffer->flags) && !buffer->unread_bytes) {
    // no unread data and none is coming, that is an EOF
    ret = AVERROR_EOF;
    goto read_error;
  }
  // have some data to read, copy them out
  to_read = (buf_size <= buffer->unread_bytes) ? buf_size : buffer->unread_bytes;
  end_offset = (buffer->index + buffer->read_bytes) % STREAM_BUFFER_BYTES;
  trailing = STREAM_BUFFER_BYTES - end_offset;
  first_copy = (trailing >= to_read) ? to_read : trailing;
  memcpy(buf, buffer->data + end_offset, first_copy);
  second_copy = to_read - first_copy;
  memcpy(buf + first_copy, buffer->data, second_copy);
  // update buffer
  buffer->read_bytes += to_read;
  buffer->unread_bytes -= to_read;
  pthread_mutex_unlock(&buffer->mutex);
  pthread_cond_signal(&buffer->condition_get);
  return to_read;

read_error:
  pthread_mutex_unlock(&buffer->mutex);
  return ret;
}

static int64_t seek_to(StreamBuffer *buffer, int64_t pos)
{
  int64_t available = buffer->read_bytes + buffer->unread_bytes;
  int64_t delta = pos - buffer->index;
  if (delta < 0) {
    // attempt to seek before the start of the buffer
    return -1;
  }
  if (available < delta) {
    // attempt to seek after the end of the buffer
    return -1;
  }

  // execute seek
  buffer->read_bytes = delta;
  buffer->unread_bytes = available - delta;
  return pos;
}

static int64_t buffer_seek_function(void *user_data, int64_t pos, int whence)
{
  StreamBuffer *buffer = (StreamBuffer *)user_data;
  // remove force flag
  whence &= ~AVSEEK_FORCE;
  pthread_mutex_lock(&buffer->mutex);
  int ret;
  if (buffer->flags & STREAM_ERROR) {
    // there was an error, emulate FFmpeg behavior
    ret = ffmpeg_error(buffer->error);
    goto seek_finish;
  }
  // FFmpeg ORs in some extra flags, so have to use AND
  if (AVSEEK_SIZE & whence) {
    if (buffer->flags & END_OF_STREAM) {
      // already have all the data so can say
      ret = buffer->index + buffer->read_bytes + buffer->unread_bytes;
      goto seek_finish;
    } else {
      // not supported, because we cannot be sure how many bytes will arrive over
      // queue
      ret = -1;
    }
    goto seek_finish;
  }
  if (SEEK_END == whence) {
    if (END_OF_STREAM & buffer->flags) {
      // possible, because we reached end of stream already
      ret = seek_to(buffer, buffer->index + buffer->read_bytes + buffer->unread_bytes + pos);
    } else {
      // can't do that because we haven't seen the end yet
      ret = -1;
    }
  } else if (SEEK_SET == whence) {
    ret = seek_to(buffer, pos);
  }  else if (SEEK_CUR == whence) {
    ret = seek_to(buffer, buffer->index + pos);
  }

seek_finish:
  pthread_mutex_unlock(&buffer->mutex);
  pthread_cond_signal(&buffer->condition_get);
  return ret;
}

int buffer_setup_as_input(StreamBuffer *buffer, AVFormatContext *ctx)
{
  // IMPORTANT: I am not sure if ffmpeg documentation states that explicitly,
  // but the memory of ctx->pb as well as its io_buffer seem to be released when
  // ctx will get closed. I tried otherwise and got "double free" errors
#define BUFFER_SIZE 4096
  void *io_buffer = av_malloc(BUFFER_SIZE);
  if (!io_buffer) return -1;
  ctx->pb = avio_alloc_context(
    io_buffer, BUFFER_SIZE,  // buffer and size
    0,                       // do not write, just read
    buffer,                  // pass buffer as user data
    buffer_read_function,
    NULL,                    // no write function supplied
    buffer_seek_function);
  if (!ctx->pb) return -1;
  ctx->flags |= AVFMT_FLAG_CUSTOM_IO;
  return 0;
}

void buffer_reset(StreamBuffer *buffer)
{
  buffer->index = buffer->read_bytes = buffer->unread_bytes = 0;
  buffer->flags = 0;
  buffer->error = 0;
}

void buffer_put_bytes(StreamBuffer *buffer, uint8_t *bytes, int64_t size)
{
  int64_t space, end, end_offset, trailing, first_copy, second_copy, deficit;
  pthread_mutex_lock(&buffer->mutex);
  while (!remaining(buffer)) {
    // wait until there is some free(able) space in the buffer
    pthread_cond_wait(&buffer->condition_get, &buffer->mutex);
  }
  // now see how much we can write
  space = remaining(buffer);
  if (space < size) size = space;
  // here we know that we can write
  end = buffer->index + buffer->read_bytes + buffer->unread_bytes;
  end_offset = end % STREAM_BUFFER_BYTES;
  // be careful to wrap around write
  trailing = STREAM_BUFFER_BYTES - end_offset;
  first_copy = (size <= trailing) ? size : trailing;
  memcpy(buffer->data + end_offset, bytes, first_copy);
  second_copy = size - first_copy;
  memcpy(buffer->data, bytes + first_copy, second_copy);
  // unread bytes changes obviously
  buffer->unread_bytes += size;
  // see if we should move index and change read_bytes
  deficit = STREAM_BUFFER_BYTES - buffer->read_bytes - buffer->unread_bytes;
  if (deficit < 0) {
    // yeah
    buffer->index -= deficit;
    buffer->read_bytes += deficit;
  }
  pthread_mutex_unlock(&buffer->mutex);
  // signal reader that it can proceed
  pthread_cond_signal(&buffer->condition_put);
}

void buffer_end_of_stream(StreamBuffer *buffer)
{
  pthread_mutex_lock(&buffer->mutex);
  buffer->flags = END_OF_STREAM;
  pthread_mutex_unlock(&buffer->mutex);
  pthread_cond_signal(&buffer->condition_put);
}

void buffer_error(StreamBuffer *buffer, StreamErrors error)
{
  pthread_mutex_lock(&buffer->mutex);
  // set flags to both error and end of stream to get out of any waiting loop
  buffer->flags = STREAM_ERROR | END_OF_STREAM;
  buffer->error = error;
  pthread_mutex_unlock(&buffer->mutex);
  pthread_cond_signal(&buffer->condition_put);
}

