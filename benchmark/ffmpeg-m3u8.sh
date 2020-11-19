#!/usr/bin/env bash

# This program takes the nvidia device as a cli arg
if [ -z "$1" ]
then
  echo "Expecting nvidia device id"
  exit 1
fi

# Expects segments following <input-name><seq-no>
# Puts outputs in folder out/

trap exit SIGINT

inp=in/bbb.m3u8

ffmpeg -hwaccel_device $1 -hwaccel cuvid -c:v h264_cuvid -i $inp \
  -vf fps=30,scale_cuda=w=1280:h=720 -b:v 6000k -c:v h264_nvenc -f null - \
  -vf fps=30,scale_cuda=w=1024:h=576 -b:v 1500k -c:v h264_nvenc -f null - \
  -vf fps=30,scale_cuda=w=640:h=360 -b:v 1200k -c:v h264_nvenc -f null - \
  -vf fps=30,scale_cuda=w=426:h=240 -b:v 600k -c:v h264_nvenc -f null - \
  -loglevel warning -hide_banner -y