#ifndef _LPMS_EXTRAS_H_
#define _LPMS_EXTRAS_H_

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int lpms_is_bypass_needed(char *fname);
int lpms_compare_sign_bypath(char *signpath1, char *signpath2);
int lpms_compare_sign_bybuffer(void *buffer1, int len1, void *buffer2, int len2);

#endif // _LPMS_EXTRAS_H_
