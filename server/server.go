package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"math/rand"
	"sync"

	"github.com/DCsunset/openwhisk-grpc/db"
	"github.com/DCsunset/openwhisk-grpc/indexing"
	"github.com/DCsunset/openwhisk-grpc/storage"
	"github.com/DCsunset/openwhisk-grpc/utils"
	"google.golang.org/grpc"
)

type Server struct {
	Servers          []string `json:"servers"`
	AvailableServers []string `json:"availableServers"`
	// Self address
	Self string `json:"self"`
	// Initial server
	Initial string `json:"initial"`
	// Split threshold
	Threshold int `json:"threshold"`

	lock                sync.RWMutex
	mergeFunction       map[uint64]string
	globalMergeFunction string
}

var store = storage.Store{}
var indexingService = indexing.Service{}

func (s *Server) Init() {
	store.Init()
	indexingService.Init()
	s.globalMergeFunction = ""
	s.mergeFunction = make(map[uint64]string)

	// Server configuration
	data, err := ioutil.ReadFile("./server.json")
	if err != nil {
		log.Fatalln(err)
	}
	json.Unmarshal(data, s)

	// Use initial server first
	indexingService.AddMapping(
		0,
		math.MaxUint32,
		s.Initial,
	)
}

func (self *Server) RemoveChildren(ctx context.Context, in *db.RemoveChildrenRequest) (*db.Empty, error) {
	address := indexingService.Locate(utils.KeyHash(in.Location))

	if address == self.Self {
		node := store.GetNode(in.Location)
		for _, child := range node.Children {
			store.RemoveNode(child)
		}
		node.Children = nil
		return &db.Empty{}, nil
	} else {
		// Forward request to the correct server
		conn, err := grpc.Dial(address, grpc.WithInsecure())
		if err != nil {
			return &db.Empty{}, err
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)

		return client.RemoveChildren(ctx, in)
	}
}

func (self *Server) AddChild(ctx context.Context, in *db.AddChildRequest) (*db.Node, error) {
	address := indexingService.Locate(utils.KeyHash(in.Location))

	if address == self.Self {
		node := store.AddChild(in.Location, in.Child)
		return &db.Node{
			Location: node.Location,
			Dep:      node.Dep,
			Key:      node.Key,
			Value:    node.Value,
			Children: node.Children,
		}, nil
	} else {
		// Forward request to the correct server
		conn, err := grpc.Dial(address, grpc.WithInsecure())
		if err != nil {
			return &db.Node{}, err
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)

		return client.AddChild(ctx, in)
	}
}

func (s *Server) Get(ctx context.Context, in *db.GetRequest) (*db.GetResponse, error) {
	address := indexingService.LocateKey(in.Key)

	if address == s.Self {
		value, err := store.Get(in.Key, in.Location)
		return &db.GetResponse{Value: value}, err
	} else {
		// Forward request to the correct server
		conn, err := grpc.Dial(address, grpc.WithInsecure())
		if err != nil {
			return &db.GetResponse{}, err
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)

		return client.Get(ctx, in)
	}
}

func (self *Server) distributeNodes(nodes []*db.Node) {
	nodeMapping := make(map[string][]*db.Node)

	for _, node := range nodes {
		server := indexingService.Locate(utils.KeyHash(node.Location))
		nodeMapping[server] = append(nodeMapping[server], node)
	}

	ctx := context.Background()
	for server, nodes := range nodeMapping {
		// Forward request to the correct server
		conn, err := grpc.Dial(server, grpc.WithInsecure())
		if err != nil {
			log.Fatalln(err)
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)
		for _, node := range nodes {
			client.AddNode(ctx, &db.AddNodeRequest{
				Node: node,
			})
		}
	}
}

func (s *Server) Set(ctx context.Context, in *db.SetRequest) (result *db.SetResponse, err error) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	address := indexingService.LocateKey(in.Key)

	if address == s.Self {
		loc := store.Set(in.Key, in.Value, in.Dep)
		// Add child
		if in.Dep != 0 {
			parent, _ := s.AddChild(ctx, &db.AddChildRequest{
				Location: in.Dep,
				Child:    loc,
			})

			// Trigger function if there's conflict
			if len(parent.Children) > 1 {
				merge, ok := s.mergeFunction[in.Dep]
				if !ok {
					merge = s.globalMergeFunction
				}
				if len(merge) > 0 {
					params, _ := json.Marshal(parent)
					resp := utils.CallAction(merge, params)
					var children *db.Nodes
					err := json.Unmarshal(resp, &children)
					if err != nil {
						return &db.SetResponse{Location: loc}, err
					}

					s.distributeNodes(children.Nodes)

					// Remove current children and use new children
					s.RemoveChildren(ctx, &db.RemoveChildrenRequest{
						Location: parent.Location,
					})
					for _, child := range children.Nodes {
						s.AddChild(ctx, &db.AddChildRequest{
							Location: child.Dep,
							Child:    child.Location,
						})
					}

					// Debug
					fmt.Println("[Merge]")
					indexingService.Print()
					fmt.Printf("Nodes: %d\n", store.Size)
					// store.Print()
				}
			}
		}

		if store.Size > s.Threshold && len(s.AvailableServers) > 0 {
			s.lock.RUnlock()
			s.lock.Lock()
			s.splitRange()
			s.lock.Unlock()
			s.lock.RLock()
		}

		result = &db.SetResponse{Location: loc}
	} else {
		// Forward request to the correct server
		conn, err := grpc.Dial(address, grpc.WithInsecure())
		if err != nil {
			return &db.SetResponse{}, err
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)

		result, err = client.Set(ctx, in)
		if store.Size > s.Threshold && len(s.AvailableServers) > 0 {
			s.lock.RUnlock()
			s.lock.Lock()
			s.splitRange()
			s.lock.Unlock()
			s.lock.RLock()
		}
	}

	// Debug
	fmt.Println("[Set]")
	indexingService.Print()
	fmt.Printf("Nodes: %d\n", store.Size)
	// store.Print()
	return result, nil
}

// [l, m] [m+1, r]
func (s *Server) Split(ctx context.Context, in *db.SplitRequest) (*db.Empty, error) {
	indexingService.RemoveMapping(in.Left, in.Right)
	indexingService.AddMapping(in.Left, in.Mid, in.LeftServer)
	indexingService.AddMapping(in.Mid+1, in.Right, in.RightServer)

	// Remove from available servers
	for i, server := range s.AvailableServers {
		if server == in.LeftServer || server == in.RightServer {
			l := len(s.AvailableServers)
			s.AvailableServers[i] = s.AvailableServers[l-1]
			s.AvailableServers = s.AvailableServers[:l-1]
			break
		}
	}

	// Debug
	fmt.Println("[Split]")
	indexingService.Print()
	fmt.Printf("Nodes: %d\n", store.Size)
	// store.Print()

	return &db.Empty{}, nil
}

// Split based on key range
// FIXME: multiple servers might split at the same
func (s *Server) splitRange() {
	left, right := indexingService.Range(s.Self)
	if left == right {
		return
	}

	number := len(s.AvailableServers)
	if number == 0 {
		return
	}
	server := s.AvailableServers[rand.Intn(number)]

	conn, err := grpc.Dial(server, grpc.WithInsecure())
	if err != nil {
		log.Fatalln(err)
	}
	defer conn.Close()
	client := db.NewDbServiceClient(conn)
	ctx := context.Background()
	// Acquire lock first
	resp, err := client.SetIndexingLock(ctx, &db.SetIndexingLockRequest{Lock: true})
	if err != nil {
		log.Fatalln(err)
	}
	if !resp.Success {
		return
	}

	mid := uint32((uint64(left) + uint64(right)) / 2)

	var keys []uint32
	for i, node := range store.Nodes {
		if i == 0 || node.Key == "" {
			continue
		}
		keys = append(keys, utils.KeyHash(node.Location))
	}

	le := 0
	greater := 0
	for _, key := range keys {
		if key > mid {
			greater += 1
		} else if key <= mid {
			le += 1
		}
	}

	// Debug
	fmt.Println("[SplitRange]")
	utils.Print(s.AvailableServers)
	fmt.Println()

	var leftServer, rightServer string
	var results []*db.Node
	if greater >= le {
		i := 0
		for _, node := range store.Nodes {
			if node.Key == "" {
				continue
			}
			if keys[i] <= mid {
				results = append(results, &db.Node{
					Location: node.Location,
					Dep:      node.Dep,
					Key:      node.Key,
					Value:    node.Value,
					Children: node.Children,
				})
			}
			i += 1
		}
		rightServer = s.Self
		leftServer = server
	} else {
		i := 0
		for _, node := range store.Nodes {
			if node.Key == "" {
				continue
			}
			if keys[i] > mid {
				results = append(results, &db.Node{
					Location: node.Location,
					Dep:      node.Dep,
					Key:      node.Key,
					Value:    node.Value,
					Children: node.Children,
				})
			}
			i += 1
		}
		rightServer = server
		leftServer = s.Self
	}

	// Debug
	fmt.Printf("AddNodes: %d\n", len(results))
	fmt.Printf("Address: %s\n", server)

	for _, node := range results {
		_, err = client.AddNode(ctx, &db.AddNodeRequest{
			Node: node,
		})
		if err != nil {
			log.Fatalln(err)
		}
	}

	// Transfer merge function
	for _, node := range results {
		f, ok := s.mergeFunction[node.Location]
		if ok {
			_, err := client.SetMergeFunction(ctx, &db.SetMergeFunctionRequest{
				Location: node.Location,
				Name:     f,
			})
			if err != nil {
				log.Fatalln(err)
			}
			delete(s.mergeFunction, node.Location)
		}
	}

	// Update indexing server
	request := &db.SplitRequest{
		Left:        left,
		Right:       right,
		Mid:         mid,
		LeftServer:  leftServer,
		RightServer: rightServer,
	}

	for _, addr := range s.Servers {
		if addr == s.Self {
			_, err := s.Split(ctx, request)
			if err != nil {
				log.Fatalln(err)
			}
		} else if addr == server {
			_, err := client.Split(ctx, request)
			if err != nil {
				log.Fatalln(err)
			}
		} else {
			// Forward request to all servers
			conn, err := grpc.Dial(addr, grpc.WithInsecure())
			if err != nil {
				log.Fatalln(err)
			}
			defer conn.Close()
			client := db.NewDbServiceClient(conn)

			_, err = client.Split(context.Background(), request)
			if err != nil {
				log.Fatalln(err)
			}
		}
	}

	// Remove nodes after range has been updated
	for _, node := range results {
		store.RemoveNode(node.Location)
	}

	_, err = client.SetIndexingLock(ctx, &db.SetIndexingLockRequest{
		Lock: false,
	})
	if err != nil {
		log.Fatalln(err)
	}
}

func (s *Server) AddNode(ctx context.Context, in *db.AddNodeRequest) (*db.Empty, error) {
	store.AddNode(in.Node)
	// Debug
	fmt.Println("[AddNodes]")
	indexingService.Print()
	fmt.Printf("Nodes: %d\n", store.Size)

	return &db.Empty{}, nil
}

func (self *Server) SetMergeFunction(ctx context.Context, in *db.SetMergeFunctionRequest) (*db.Empty, error) {
	// FIXME: find the right server to add merge function

	if len(in.Name) == 0 {
		delete(self.mergeFunction, in.Location)
	} else {
		self.mergeFunction[in.Location] = in.Name
	}
	return &db.Empty{}, nil
}

func (self *Server) SetGlobalMergeFunction(ctx context.Context, in *db.SetGlobalMergeFunctionRequest) (*db.Empty, error) {
	for _, addr := range self.Servers {
		if addr == self.Self {
			self.globalMergeFunction = in.Name
		} else {
			// Forward request to all servers
			conn, err := grpc.Dial(addr, grpc.WithInsecure())
			if err != nil {
				log.Fatalln(err)
			}
			defer conn.Close()
			client := db.NewDbServiceClient(conn)

			client.SetGlobalMergeFunction(ctx, in)
		}
	}
	return &db.Empty{}, nil
}

func (self *Server) GetNode(ctx context.Context, in *db.GetNodeRequest) (*db.Node, error) {
	address := indexingService.Locate(utils.KeyHash(in.Location))

	if address == self.Self {
		node := store.GetNode(in.Location)
		if node == nil {
			return &db.Node{}, fmt.Errorf("Location %x not found", in.Location)
		}

		return &db.Node{
			Location: node.Location,
			Dep:      node.Dep,
			Key:      node.Key,
			Value:    node.Value,
			Children: node.Children,
		}, nil
	} else {
		// Forward request to the correct server
		conn, err := grpc.Dial(address, grpc.WithInsecure())
		if err != nil {
			return &db.Node{}, err
		}
		defer conn.Close()
		client := db.NewDbServiceClient(conn)

		return client.GetNode(ctx, in)
	}
}

func (self *Server) SetIndexingLock(ctx context.Context, in *db.SetIndexingLockRequest) (*db.SetIndexingLockResponse, error) {
	if in.Lock {
		if !indexingService.Lock {
			indexingService.Lock = true
			return &db.SetIndexingLockResponse{
				Success: true,
			}, nil
		} else {
			return &db.SetIndexingLockResponse{
				Success: false,
			}, nil
		}
	} else {
		indexingService.Lock = false
		return &db.SetIndexingLockResponse{
			Success: true,
		}, nil
	}
}
