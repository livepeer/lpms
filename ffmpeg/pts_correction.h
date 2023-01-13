#ifndef _LPMS_PTS_CORRECTION_H_
#define _LPMS_PTS_CORRECTION_H_

#include <stdint.h>

enum first_pts_status {
  FIRST_PTS_NOVALUE = 0,
  FIRST_PTS_CAPTURING,
  FIRST_PTS_OFFSET_CALCULATED
};

// All fields default value is 0
struct first_pts {
  enum first_pts_status status;
  int64_t pts_value;
  int64_t offset;
};

void    capture_pts(struct first_pts * p, int64_t pts);
int64_t get_first_pts_offset(struct first_pts * p, int64_t pts);

#endif // _LPMS_PTS_CORRECTION_H_
