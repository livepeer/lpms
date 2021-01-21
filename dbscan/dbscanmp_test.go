package dbscan

import (
	"fmt"
	"log"
	"math"
	"testing"
)

type MultiPoint struct {
	index int
	pos   []float64
}

func (s MultiPoint) DistanceTo(c Point) float64 {
	distance := 0.0
	dotproduct := 0.0
	s1 := 0.0
	s2 := 0.0
	if len(c.(MultiPoint).pos) != len(s.pos) {
		distance = 2.0
		return distance
	}

	for i := 0; i < len(c.(MultiPoint).pos); i++ {
		dotproduct += c.(MultiPoint).pos[i] * s.pos[i]
		s1 += s.pos[i] * s.pos[i]
		s2 += c.(MultiPoint).pos[i] * c.(MultiPoint).pos[i]
	}

	distance = dotproduct / (math.Sqrt(s1) * math.Sqrt(s2))

	//log.Println("distance:", 1.0-distance)
	return 1.0 - distance
}

func (s MultiPoint) Name() string {
	return fmt.Sprint(s.index)
}

func TestMpPutAll(t *testing.T) {
	testMap := make(map[string]Point)
	clusterList := []Point{
		MultiPoint{index: 1, pos: []float64{1.0, 2.0, 3.0}},
		MultiPoint{index: 2, pos: []float64{1.0, 2.0, 3.0}},
	}
	merge(testMap, clusterList...)
	mapSize := len(testMap)
	if mapSize != 2 {
		t.Errorf("Map does not contain expected size 2 but was %d", mapSize)
	}
}

//Test find neighbour function
func TestMpFindNeighbours(t *testing.T) {
	log.Println("Executing TestMpFindNeighbours")
	clusterList := []Point{
		MultiPoint{index: 1, pos: []float64{29328, 97760, 160552, 197400, 207740, 321292, 541252, 579792, 625476, 681312}},
		MultiPoint{index: 2, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 3, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 4, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 5, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
	}

	eps := 0.0015
	neighbours := findNeighbours(clusterList[0], clusterList, eps)

	log.Println("neighbours counts:", len(neighbours))

	if 4 != len(neighbours) {
		t.Error("Mismatched neighbours counts")
	}
}

func TestMpExpandCluster(t *testing.T) {
	log.Println("Executing TestMpExpandCluster")
	expected := 5
	clusterList := []Point{
		MultiPoint{index: 1, pos: []float64{29328, 97760, 160552, 197400, 207740, 321292, 541252, 579792, 625476, 681312}},
		MultiPoint{index: 2, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 3, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 4, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 5, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
	}

	eps := 0.0015
	minPts := 2
	visitMap := make(map[string]bool)
	cluster := make([]Point, 0)
	cluster = expandCluster(cluster, clusterList, visitMap, minPts, eps)
	if expected != len(cluster) {
		t.Error("Mismatched cluster counts")
	}
}

func TestMpCluster(t *testing.T) {
	clusters := Cluster(2, 0.0015,
		MultiPoint{index: 1, pos: []float64{29328, 97760, 160552, 197400, 207740, 321292, 541252, 579792, 625476, 681312}},
		MultiPoint{index: 2, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 3, pos: []float64{29328, 97948, 160552, 197400, 207740, 321668, 541252, 579604, 623596, 680372}},
		MultiPoint{index: 4, pos: []float64{883976, 1025728, 1210720, 1263924, 1554196, 1962344, 2316724, 3441528, 0, 0}},
		MultiPoint{index: 5, pos: []float64{896948, 1030616, 1216360, 1256780, 1595744, 2021564, 2394744, 3639304, 0, 0}},
		MultiPoint{index: 6, pos: []float64{896948, 1030616, 1216360, 1256780, 1595744, 2021564, 2394744, 3639304, 0, 0}},
	)

	log.Println("cluster counts:", len(clusters))

	if 2 == len(clusters) {
		if 3 != len(clusters[0]) || 3 != len(clusters[1]) {
			t.Error("Mismatched cluster member counts")
		} else {
			log.Println("cluster names:", clusters[0][0].Name(), clusters[0][1].Name(), clusters[0][2].Name())
			log.Println("cluster names:", clusters[1][0].Name(), clusters[1][1].Name(), clusters[1][2].Name())
		}
	} else {
		t.Error("Mismatched cluster counts")
	}
}

func TestMpClusterNoData(t *testing.T) {
	log.Println("Executing TestMpClusterNoData")

	clusters := Cluster(3, 1.0)
	if 0 != len(clusters) {
		t.Error("Mismatched cluster counts")
	}
}
