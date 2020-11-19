#include <stdio.h>
#include <pthread.h>

#include <unistd.h> // sleep
#include <signal.h> // signal
#include <time.h>
#include <sys/time.h>

#include "/home/josh/compiled/include/libavformat/avformat.h"
#include "/home/josh/code/src/github.com/livepeer/lpms/ffmpeg/lpms_ffmpeg.h"

static int done = 0;

typedef struct {
  char in[64];
  char out1[64], out2[64], out3[64], out4[64];
  char device[64];
  int nb, conc;
} data;

void sig_handler(int signo) {
  fprintf(stderr, "Received signal; stopping transcodes\n");
  done = 1;
}

void print(int seg, int i, struct timeval *start) {
  time_t rawtime;
  struct tm timeinfo;
  struct timeval end;
  unsigned long long ms;
  char buf[64];
  time(&rawtime);
  localtime_r(&rawtime, &timeinfo);
  gettimeofday(&end, NULL);
  asctime_r(&timeinfo, buf);
  buf[strlen(buf)-1] = '\0'; // remove trailing newline
  ms = 1000*(end.tv_sec - start->tv_sec)+(end.tv_usec - start->tv_usec) / 1000;
  printf("%s,%d,%d,%llu\n", buf, i, seg, ms);
}

void run_segment(int seg, input_params *inp, output_params *out, int nb_out, int thr_idx) {
  int i;
  sprintf(inp->fname, "in/bbb%d.ts", seg);

  for (i = 0; i < nb_out; i++) {
    sprintf(out[i].fname, "out/c_conc_%s_%d_%d.ts", inp->device, seg, i);
  }

  int ret;
  output_results res[4];
  output_results decoded_res;
  struct timeval start;
  gettimeofday(&start, NULL);
  ret = lpms_transcode(inp, out, (&res[0]), 4, &decoded_res);
  if (ret != 0) {
    fprintf(stderr, "Error transcoding ret=%d stream=%d segment=%d\n",
      ret, thr_idx, seg);
    return;
  }
  print(seg, thr_idx, &start);
}

void stream(data *d) {
  int i;
  struct transcode_thread *t = lpms_transcode_new();
  input_params inp = {
    handle: t,
    fname: d->in,
    device: d->device,
    hw_type: AV_HWDEVICE_TYPE_CUDA
  };
  output_params out[] = {{
    fname: d->out1,
    video: { name: "h264_nvenc" },
    audio: { name: "copy" },
    vfilters: "fps=30/1,scale_cuda=w=1280:h=720",
    //vfilters: "fps=30/1,scale_npp=w=1280:h=720",
    fps: {30, 1}
  },{
    fname: d->out2,
    video: { name: "h264_nvenc" },
    audio: { name: "copy" },
    vfilters: "fps=30/1,scale_cuda=w=1024:h=576",
    //vfilters: "fps=30/1,scale_npp=w=1024:h=576",
    fps: {30, 1}
  },{
    fname: d->out3,
    video: { name: "h264_nvenc" },
    audio: { name: "copy" },
    vfilters: "fps=30/1,scale_cuda=w=640:h=360",
    //vfilters: "fps=30/1,scale_npp=w=640:h=360",
    fps: {30, 1}
  },{
    fname: d->out4,
    video: { name: "h264_nvenc" },
    audio: { name: "copy" },
    vfilters: "fps=30/1,scale_cuda=w=426:h=240",
    //vfilters: "fps=30/1,scale_npp=w=426:h=240",
    fps: {30, 1}
  }};
  int segs = rand() % 30;
  for (i = 0; i < segs; i++) {
    if (done) break;
    run_segment(i, &inp, out, sizeof(out) / sizeof(output_params), d->nb);
  }
  lpms_transcode_stop(t);
}

void* run(void *opaque) {
  data *d = (data*)opaque;
  int nb = d->nb, i = 0;
  while (!done) {
    d->nb = (i * d->conc) + nb;
    stream(d);
    i += 1;
  }
  return NULL;
}

int main(int argc, char **argv) {

  if (argc < 2) {
    fprintf(stderr, "Usage: %s <concurrency>\n", argv[0]);
    return 1;
  }

  signal(SIGINT, sig_handler);
  lpms_init();

  int i;
  int conc = atoi(argv[1]);
  pthread_t threads[128];
  data datas[128];

  if (conc <= 0 || conc > 128) {
    fprintf(stderr, "Concurrency must be between 1 and 128\n");
    return 1;
  }

  printf("time,stream,segment,length\n");
  for (i = 0; i < conc; i++) {
    data *d = &datas[i];
    snprintf(d->out1, sizeof d->out1, "out/conc_%d_720.ts", i);
    snprintf(d->out2, sizeof d->out2, "out/conc_%d_576.ts", i);
    snprintf(d->out3, sizeof d->out3, "out/conc_%d_360.ts", i);
    snprintf(d->out4, sizeof d->out4, "out/conc_%d_240.ts", i);
    snprintf(d->device, sizeof d->device, "%d", i % 8);
    d->nb = i;
    d->conc = conc;
    //printf("Processing %d on device %s\n", i, d->device);
    pthread_create(&threads[i], NULL, run, (void*)d);
    sleep(1);
  }

  for (i = 0; i < conc; i++) {
    pthread_join(threads[i], NULL);
  }

  return 0;
}
