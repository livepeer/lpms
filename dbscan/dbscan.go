package dbscan

// Point represents a cluster point which can measure distance to another point.
type Point interface {
	Name() string
	DistanceTo(pt Point, bl2 bool) float64
}

// Cluster clusters the points by using DBSCAN method. It requires two parameters: epsilon and the
// minimum number of points required to form a dense region (minDensity).
func Cluster(minDensity int, epsilon float64, bl2 bool, points ...Point) (clusters [][]Point) {
	visited := make(map[string]bool, len(points))
	for _, point := range points {
		neighbours := findNeighbours(point, points, epsilon, bl2)
		if len(neighbours)+1 >= minDensity {
			visited[point.Name()] = true
			cluster := []Point{point}
			cluster = expandCluster(cluster, neighbours, visited, minDensity, epsilon, bl2)

			if len(cluster) >= minDensity {
				clusters = append(clusters, cluster)
			}
		} else {
			visited[point.Name()] = false
		}
	}
	return clusters
}

// Finds the neighbours from given array, depends on epsolon , which determines
// the distance limit from the point
func findNeighbours(point Point, points []Point, epsilon float64, bl2 bool) []Point {
	neighbours := make([]Point, 0)
	for _, potNeigb := range points {
		if point.Name() != potNeigb.Name() && potNeigb.DistanceTo(point, bl2) <= epsilon {
			neighbours = append(neighbours, potNeigb)
		}
	}
	return neighbours
}

// Try to expand existing clutser
func expandCluster(cluster, neighbours []Point, visited map[string]bool, minDensity int, eps float64, bl2 bool) []Point {
	seed := make([]Point, len(neighbours))
	copy(seed, neighbours)

	// Create a new set for merging
	set := make(map[string]Point, len(cluster)+len(neighbours))
	merge(set, cluster...)

	// Merge all of the points
	for _, point := range seed {
		clustered, isVisited := visited[point.Name()]
		if !isVisited {
			currentNeighbours := findNeighbours(point, seed, eps, bl2)
			if len(currentNeighbours)+1 >= minDensity {
				visited[point.Name()] = true
				merge(set, currentNeighbours...)
			}
		}

		if isVisited && !clustered {
			visited[point.Name()] = true
			merge(set, point)
		}
	}

	// Flatten and return the cluster
	merged := make([]Point, 0, len(set))
	for _, v := range set {
		merged = append(merged, v)
	}
	return merged
}

func merge(dst map[string]Point, src ...Point) {
	for _, v := range src {
		dst[v.Name()] = v
	}
}
