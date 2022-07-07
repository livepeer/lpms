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

void queue_push_back(StreamBufferQueue *queue, StreamBuffer *buffer)
{
  // add new element first...
  pthread_mutex_lock(&queue->mutex);
  if (queue->front) {
    // adding first element to empty queue
    queue->front = queue->back = buffer;
  } else {
    // adding element at the back
    queue->back->previous = buffer;
    queue->back = buffer;
  }
  buffer->previous = NULL;
  pthread_mutex_unlock(&queue->mutex);
  // ...then signal condition variable
  pthread_cond_signal(&queue->condition);
}

// helper function - wait for queue to contain some elements
// call this function only when already owning queue->mutex
static void queue_wait(StreamBufferQueue *queue)
{
  while (!queue->front) {
    pthread_cond_wait(&queue->condition, &queue->mutex);
  }
}

const StreamBuffer *queue_peek_front(StreamBufferQueue *queue)
{
  const StreamBuffer *front;
  pthread_mutex_lock(&queue->mutex);
  // wait for queue not to be empty
  queue_wait(queue);
  front = queue->front;
  pthread_mutex_unlock(&queue->mutex);
  return front;
}

void queue_pop_front(StreamBufferQueue *queue)
{
  StreamBuffer *element;
  pthread_mutex_lock(&queue->mutex);
  // wait for queue not to be empty
  queue_wait(queue);
  // take away front element
  element = queue->front;
  queue->front = element->previous;
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
    queue->front = queue->front->previous;
    free(tmp->data);
    free(tmp);
  }
  pthread_mutex_unlock(&queue->mutex);
}
