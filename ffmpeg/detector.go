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
	ID   int    // unique ID within LPMS per class
	Name string // unique Name within LPMS per class
}

type SceneClassificationProfile struct {
	SampleRate uint
	ModelPath  string
	Input      string
	Output     string
	Classes    []DetectorClass
}

func (p *SceneClassificationProfile) Type() DetectorType {
	return SceneClassification
}

var (
	DSceneAdultSoccer = SceneClassificationProfile{
		SampleRate: 30,
		ModelPath:  "tasmodel.pb",
		Input:      "input_1",
		Output:     "Identity",
		Classes:    []DetectorClass{{ID: 0, Name: "adult"}, {ID: 1, Name: "soccer"}},
	}
	DSceneViolence = SceneClassificationProfile{
		SampleRate: 30,
		ModelPath:  "tviomodel.pb",
		Input:      "input_1",
		Output:     "reshape_3/Reshape",
		Classes:    []DetectorClass{{ID: 2, Name: "violence"}},
	}
)

var SceneClassificationProfileLookup = map[string]SceneClassificationProfile{
	"adult":    DSceneAdultSoccer,
	"soccer":   DSceneAdultSoccer,
	"violence": DSceneViolence,
}

var DetectorClassIDLookup = map[string]int{
	"adult":    0,
	"soccer":   1,
	"violence": 2,
}

type DetectData interface {
	Type() DetectorType
}

type SceneClassificationData map[int]float64

func (scd SceneClassificationData) Type() DetectorType {
	return SceneClassification
}
