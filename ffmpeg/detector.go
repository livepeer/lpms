package ffmpeg

type DetectorType int

const (
	SceneClassification = iota
	// Example for future:
	// ObjectDetection
)

type DetectorProfile interface {
	Type() DetectorType
}

type DetectorClass struct {
	ID   int
	Name string
}

type SceneClassificationProfile struct {
	SampleRate uint
	ModelPath  string
	Input      string
	Output     string
	Threshold  float32
	Classes    []DetectorClass
}

func (p *SceneClassificationProfile) Type() DetectorType {
	return SceneClassification
}

type DetectData interface {
	Type() DetectorType
}

type SceneClassificationData map[int]float64

func (scd SceneClassificationData) Type() DetectorType {
	return SceneClassification
}
