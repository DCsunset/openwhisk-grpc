package storage

import (
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"github.com/DCsunset/openwhisk-grpc/db"
	"github.com/DCsunset/openwhisk-grpc/utils"
)

type Node struct {
	Location uint64 // The location of the key
	Dep      uint64
	Children []uint64
	Key      string
	Value    string
}

type Store struct {
	Nodes []Node // all nodes
	// Map hash locations to memory locations
	MemLocation map[uint64]int
	lock        sync.RWMutex
	Size        int // Size of valid nodes
}

func (s *Store) Init() {
	if len(s.Nodes) == 0 {
		// Create a root and map first
		s.MemLocation = make(map[uint64]int)
		root := Node{
			Dep:      math.MaxUint64,
			Location: 0,
			Key:      "",
		}
		s.Nodes = append(s.Nodes, root)
		s.MemLocation[0] = 0
	}
}

func (s *Store) newNode(location uint64, dep uint64, key string, value string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	s.Size += 1
	node := Node{
		Location: location,
		Dep:      dep,
		Key:      key,
		Children: nil,
		Value:    value,
	}

	s.Nodes = append(s.Nodes, node)
	memLoc := len(s.Nodes) - 1

	s.MemLocation[location] = memLoc
}

func (s *Store) Get(key string, loc uint64) (string, error) {
	// FIXME: Similuate disk
	time.Sleep(time.Millisecond * 10)

	var node *Node
	node = s.GetNode(loc)

	// Find till root
	for {
		if node.Key == key {
			return node.Value, nil
		}
		if node.Dep == math.MaxUint64 {
			break
		}
		node = s.GetNode(node.Dep)
	}
	return "", fmt.Errorf("Key %s not found", key)
}

type Data struct {
	Key   string
	Value string
	Dep   int64
}

func (self *Store) AddChild(location uint64, child uint64) *Node {
	node := self.GetNode(location)
	node.Children = append(node.Children, child)
	return node
}

func (s *Store) Set(key string, value string, dep uint64) uint64 {
	// FIXME: Similuate disk
	time.Sleep(time.Millisecond * 10)

	// Use random number + key hash
	loc := uint64(rand.Uint32()) + (uint64(utils.Hash2Uint(utils.Hash([]byte(key)))) << 32)
	s.newNode(loc, dep, key, value)

	return loc
}

func CreateNode(key, value string, dep uint64) *db.Node {
	// Use random number + key hash
	loc := uint64(rand.Uint32()) + (uint64(utils.Hash2Uint(utils.Hash([]byte(key)))) << 32)
	return &db.Node{
		Location: loc,
		Dep:      dep,
		Key:      key,
		Value:    value,
		Children: nil,
	}
}

func (s *Store) GetNode(loc uint64) *Node {
	memLoc, ok := s.MemLocation[loc]
	if !ok {
		return nil
	}
	return &s.Nodes[memLoc]
}

func (s *Store) AddNode(node *db.Node) {
	s.newNode(node.Location, node.Dep, node.Key, node.Value)
}

func (s *Store) RemoveNode(location uint64) {
	for i, node := range s.Nodes {
		if node.Location == location {
			s.Nodes[i] = Node{
				Key: "",
			}
			s.Size -= 1
			return
		}
	}
}

func (s *Store) Print() {
	fmt.Println("Nodes:")
	for _, node := range s.Nodes {
		if len(node.Key) > 0 {
			fmt.Printf("%s (Dep: %x, Chilren: %s)\n", node.Key, node.Dep, utils.ToString(node.Children))
		}
	}
}
