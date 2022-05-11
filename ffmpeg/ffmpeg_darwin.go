//go:build darwin
// +build darwin

package ffmpeg

// #cgo LDFLAGS: -framework Foundation -framework Security
import "C"
