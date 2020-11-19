#!/usr/bin/env bash

# This program takes the number of concurrents as a cli arg
if [ -z "$1" ]
then
  echo "Expecting concurrent count"
  exit 1
fi

trap exit SIGINT

for i in $(seq 0 $(($1-1)) )
do
  echo "Starting ffmpeg stream $i with device $(( $i % 8 ))"
  ./ffmpeg-m3u8 $(( $i % 8 )) &
done

wait
