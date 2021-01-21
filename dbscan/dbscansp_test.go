package dbscan

import (
	"fmt"
	"log"
	"math"
	"testing"
)

type SinglePoint struct {
	position float64
}

func (s SinglePoint) DistanceTo(c Point) float64 {
	distance := math.Abs(c.(SinglePoint).position - s.position)
	return distance
}

func (s SinglePoint) Name() string {
	return fmt.Sprint(s.position)
}

func TestPutAll(t *testing.T) {
	testMap := make(map[string]Point)
	clusterList := []Point{
		SinglePoint{10},
		SinglePoint{12},
	}
	merge(testMap, clusterList...)
	mapSize := len(testMap)
	if mapSize != 2 {
		t.Errorf("Map does not contain expected size 2 but was %d", mapSize)
	}
}

//Test find neighbour function
func TestFindNeighbours(t *testing.T) {
	log.Println("Executing TestFindNeighbours")
	clusterList := []Point{
		SinglePoint{0},
		SinglePoint{1},
		SinglePoint{-1},
		SinglePoint{1.5},
		SinglePoint{-0.5},
	}

	eps := 1.01
	neighbours := findNeighbours(clusterList[0], clusterList, eps)
	if 3 != len(neighbours) {
		t.Error("Mismatched neighbours counts")
	}
}

func TestExpandCluster(t *testing.T) {
	log.Println("Executing TestExpandCluster")
	expected := 4
	clusterList := []Point{
		SinglePoint{0},
		SinglePoint{1},
		SinglePoint{2},
		SinglePoint{2.1},
		SinglePoint{5},
	}

	eps := 1.0
	minPts := 1
	visitMap := make(map[string]bool)
	cluster := make([]Point, 0)
	cluster = expandCluster(cluster, clusterList, visitMap, minPts, eps)
	if expected != len(cluster) {
		t.Error("Mismatched cluster counts")
	}
}

func TestCluster(t *testing.T) {
	clusters := Cluster(2, 1.0,
		SinglePoint{1},
		SinglePoint{0.5},
		SinglePoint{0},
		SinglePoint{5},
		SinglePoint{4.5},
		SinglePoint{4})

	if 2 == len(clusters) {
		if 3 != len(clusters[0]) || 3 != len(clusters[1]) {
			t.Error("Mismatched cluster member counts")
		}
	} else {
		t.Error("Mismatched cluster counts")
	}
}

func TestClusterNoData(t *testing.T) {
	log.Println("Executing TestClusterNoData")

	clusters := Cluster(3, 1.0)
	if 0 != len(clusters) {
		t.Error("Mismatched cluster counts")
	}
}
