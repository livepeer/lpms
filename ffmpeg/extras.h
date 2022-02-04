#ifndef _LPMS_EXTRAS_H_
#define _LPMS_EXTRAS_H_

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int lpms_get_codec_info(char *fname, char *out_video_codec, char *out_audio_codec, int *out_pixel_format);
int lpms_compare_sign_bypath(char *signpath1, char *signpath2);
int lpms_compare_sign_bybuffer(void *buffer1, int len1, void *buffer2, int len2);
int lpms_compare_video_bypath(char *vpath1, char *vpath2);
int lpms_compare_video_bybuffer(void *buffer1, int len1, void *buffer2, int len2);

#endif // _LPMS_EXTRAS_H_
