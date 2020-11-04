#ifndef _LPMS_EXTRAS_H_
#define _LPMS_EXTRAS_H_

int lpms_rtmp2hls(char *listen, char *outf, char *ts_tmpl, char *seg_time, char *seg_start);
int lpms_is_bypass_needed(char *fname);

#endif // _LPMS_EXTRAS_H_
