#ifndef _LPMS_EXTRAS_H_
#define _LPMS_EXTRAS_H_

typedef struct s_codec_info {
  char * video_codec;
  char * audio_codec;
  int    pixel_format;
} codec_info, *pcodec_info;

typedef struct s_match_info {
  int       width;
  int       height;
  uint64_t  bit_rate;
  int       packetcount; //video total packet count
  uint64_t  timestamp;    //XOR sum of avpacket pts
  int       audiosum[256]; //Histogram of audio data
  int       md5allocsize;
  int       apacketcount; //audio packet count
  int       *pmd5array;
} match_info;

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int lpms_get_codec_info(char *fname, pcodec_info out);
int lpms_get_matchinfo(char *vpath1, match_info* info);
int lpms_compare_sign_bypath(char *signpath1, char *signpath2);
int lpms_compare_sign_bybuffer(void *buffer1, int len1, void *buffer2, int len2);
int lpms_compare_video_bypath(char *vpath1, char *vpath2);
int lpms_compare_video_bybuffer(void *buffer1, int len1, void *buffer2, int len2);
double lpms_getmatch_cost(char *vpath1, char *vpath2);

#endif // _LPMS_EXTRAS_H_
