package main

type Node struct {
	dep  int64
	data map[string]string // record current updates
}

type Store struct {
	nodes []Node // all nodes
}

func (s *Store) newNode(dep int64, data map[string]string) int64 {
	node := Node{
		dep:  -1,
		data: data,
	}
	s.nodes = append(s.nodes, node)
	return int64(len(s.nodes)) - 1
}

func (s *Store) Get(keys []string, loc int64) map[string]string {
	node := s.nodes[loc]
	data := make(map[string]string)

	// Find till root
	for node.dep != -1 {
		for _, key := range keys {
			_, ok := data[key]
			if !ok {
				value, ok := node.data[key]
				if ok {
					data[key] = value
				}
			}
		}
		node = s.nodes[node.dep]
	}
	return data
}

func (s *Store) Set(data map[string]string, virtualLoc int64, dep int64, virtualDep int64) int64 {
	newLoc := s.newNode(dep, data)
	return newLoc
}