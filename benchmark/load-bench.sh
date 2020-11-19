profiles=240p0,360p0,480p0,720p0

for i in {1..20}
do
        ./lpms-bench in/bbb.m3u8 out $i 60 $profiles nv 0
        sleep 2
done
