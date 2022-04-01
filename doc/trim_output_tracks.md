
# Trimming stream

Use `From` and `To` parameters of `TranscodeOptions` to trim encoded output.

Both audio and video tracks are trimmed from start if `From` is specified and trimmed at the end if `To` is specified. Starting point of `From` and `To` is first frame timestamp of corresponding track allowing to clip audio and video independently.

Additional check is in place to skip all audio frames preceeding first video frame.

## Usage

```go
	in := &TranscodeOptionsIn{Fname: "./input.ts"}
	out := []TranscodeOptions{{
		Profile: P144p30fps16x9,
		Oname:   "./output.mp4",
		From: 1 * time.Second,
		To:   3 * time.Second,
	}}
	res, err := Transcode3(in, out)
```

## Examples to depict `From: 2` `To: 8`

aligned input:
```
V V V V V
AAAAAAAAAA

output:
V V V 
AAAAAA
```

offsted input:
```
V V V V V     
    AAAAAAAAAA

output:
V V V     
    AAAAAA
```

offsted input:
```
    V V V V V
AAAAAAAAAA   

output:
V V V
 AAA 
```


