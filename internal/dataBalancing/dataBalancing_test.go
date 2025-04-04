package dataBalancing_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"testing"
	"time"

	pb "github.com/uimagine-admin/tunadb/api"
	"github.com/uimagine-admin/tunadb/internal/dataBalancing"
	"github.com/uimagine-admin/tunadb/internal/db"
	"github.com/uimagine-admin/tunadb/internal/gossip"
	rp "github.com/uimagine-admin/tunadb/internal/ring"
	"github.com/uimagine-admin/tunadb/internal/types"
	"github.com/uimagine-admin/tunadb/internal/utils"
	"google.golang.org/grpc"
)

type server struct {
	GossipHandler *gossip.GossipHandler
	pb.UnimplementedCassandraServiceServer
	DataDistributionHandler *dataBalancing.DistributionHandler
}

// handle incoming gossip request
func (s *server) Gossip(ctx context.Context, req *pb.GossipMessage) (*pb.GossipAck, error) {
	return s.GossipHandler.HandleGossipMessage(ctx, req)
}

// handle incoming sync request
func (s *server) SyncData(stream pb.CassandraService_SyncDataServer) error {
	for {
		// Receive messages from the stream
		req, err := stream.Recv()
		if err == io.EOF {
			// End of stream
			return nil
		}
		if err != nil {
			return fmt.Errorf("error receiving stream: %v", err)
		}

		s.DataDistributionHandler.HandleDataSync(stream.Context(), req)

		// Send a response back
		resp := &pb.SyncDataResponse{
			Status:  "success",
			Message: "Processed successfully",
		}
		if err := stream.Send(resp); err != nil {
			return fmt.Errorf("error sending stream: %v", err)
		}
	}
}

// Helper to start a gRPC server for a gossip handler
func StartNode(handler *gossip.GossipHandler, dataDistributionHandler *dataBalancing.DistributionHandler) (*grpc.Server, error) {
	grpcServer := grpc.NewServer()
	pb.RegisterCassandraServiceServer(grpcServer, &server{
		GossipHandler:           handler,
		DataDistributionHandler: dataDistributionHandler,
	})

	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", handler.NodeInfo.Port))
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %v", err)
	}

	go func() {
		if err := grpcServer.Serve(listener); err != nil {
			log.Printf("Error serving gRPC for node %s: %v", handler.NodeInfo.Name, err)
		}
	}()

	return grpcServer, nil
}

/*
Helper function to create an initial system with numNodes nodes and return the nodes and their corresponding rings
and begin gossip protocol for each node

numNodes: Number of nodes in the system
replicationFactor: Number of replicas for each node
numberOfVirtualNodes: Number of virtual nodes for each node
*/
func createInitialSystem(numNodes int, numberOfVirtualNodes uint64, replicationFactor int) ([]*types.Node, []*rp.ConsistentHashingRing, []*dataBalancing.DistributionHandler) {
	// step 0: create numNodes nodes and their corresponding rings
	nodes := make([]*types.Node, numNodes)
	nodeRings := make([]*rp.ConsistentHashingRing, numNodes)
	dataHandlers := make([]*dataBalancing.DistributionHandler, numNodes)

	for i := 0; i < numNodes; i++ {
		node := &types.Node{
			IPAddress:   "localhost",
			ID:          fmt.Sprintf("data_distribution_test_node_%d", i),
			Port:        uint64(9000 + i),
			Name:        fmt.Sprintf("cassandra-node%d", i),
			Status:      types.NodeStatusAlive,
			LastUpdated: time.Now(),
		}
		nodes[i] = node
	}

	// Step 1: Initialize the cluster with a consistent hashing ring
	for i := 0; i < numNodes; i++ {
		relativePathSaveDir := fmt.Sprintf("../db/internal/data/%s.json", nodes[i].ID)
		absolutePathSaveDir := utils.GetPath(relativePathSaveDir)

		ringView := rp.CreateConsistentHashingRing(nodes[i], numberOfVirtualNodes, replicationFactor)
		nodeRings[i] = ringView
		dataHandler := dataBalancing.NewDistributionHandler(ringView, nodes[i], absolutePathSaveDir)
		dataHandlers[i] = dataHandler
	}

	return nodes, nodeRings, dataHandlers
}

func runSystem(nodes []*types.Node, nodeRings []*rp.ConsistentHashingRing, dataHandlers []*dataBalancing.DistributionHandler, gossipFanOut int, suspectToDeadTimeout int, gossipInterval int) ([]*gossip.GossipHandler, []*grpc.Server, []*context.Context, []*context.CancelFunc) {
	gossipHandlers := make([]*gossip.GossipHandler, len(nodes))
	// Step 2: Start gossip handlers for all nodes
	for i, node := range nodes {
		gossipHandlers[i] = gossip.NewGossipHandler(node, nodeRings[i], gossipFanOut, suspectToDeadTimeout, gossipInterval, dataHandlers[i])

		// Add all nodes to the membership list
		for _, otherNode := range nodes {
			if !node.Equals(otherNode) {
				gossipHandlers[i].Membership.AddOrUpdateNode(otherNode, nodeRings[i])
			}
		}

	}

	// Step 3: Run gRPC servers for all nodes
	servers := []*grpc.Server{}
	contexts := []*context.Context{}
	cancelFuncs := []*context.CancelFunc{}
	for i, handler := range gossipHandlers {
		ctx, cancel := context.WithCancel(context.Background())
		contexts = append(contexts, &ctx)
		cancelFuncs = append(cancelFuncs, &cancel)
		server, err := StartNode(handler, dataHandlers[i])
		if err != nil {
			log.Printf("Failed to start gRPC server for node %s: %v", nodes[i].Name, err)
			defer cancel()
			server.Stop()
			return nil, nil, nil, nil
		}
		servers = append(servers, server)

	}

	// Step 4: Start gossip protocol for each node in separate goroutines
	for i, handler := range gossipHandlers {
		go handler.Start(*contexts[i], gossipFanOut)
	}

	return gossipHandlers, servers, contexts, cancelFuncs
}

func stopServers(servers []*grpc.Server, cancelContexts []*context.CancelFunc) {
	for i, server := range servers {
		server.Stop()
		defer (*cancelContexts[i])()
	}
}

func TestDataSyncInitialSystemSetUp(t *testing.T) {
	numNodes := 4
	numVirtualNodes := uint64(3)
	replicationFactor := 2
	gossipFanOut := 2
	suspectToDeadTimeout := 8
	gossipInterval := 3

	// Step 0: Create an initial system structure
	nodes, nodeRings, dataHandlers := createInitialSystem(numNodes, numVirtualNodes, replicationFactor)

	records := []map[string]string{
		{
			"pageID":     "19",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.847637592Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.851746508Z",
			"created_at": "2024-12-07T07:41:19.851749008Z",
			"hashkey":    "13502972256853596262",
		},
		{
			"pageID":     "86",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.663822341Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.665629591Z",
			"created_at": "2024-12-07T07:41:19.665632425Z",
			"hashkey":    "4392469504148276032",
		},
		{
			"pageID":     "42",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.802749175Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.803729092Z",
			"created_at": "2024-12-07T07:41:19.803732425Z",
			"hashkey":    "13154972877196513132",
		},
		{
			"pageID":     "93",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.81675455Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.8174478Z",
			"created_at": "2024-12-07T07:41:19.8174488Z",
			"hashkey":    "14458771382211144428",
		},
	}

	// Step 1: create data files for each node
	for i, node := range nodes {

		rows := []db.Row{}

		newEvent := db.Row{
			PageId:      records[i]["pageID"],
			ComponentId: records[i]["element"],
			Timestamp:   records[i]["timestamp"],
			Event:       records[i]["event"],
			UpdatedAt:   records[i]["updated_at"],
			CreatedAt:   records[i]["created_at"],
			HashKey:     records[i]["hashkey"],
		}

		rows = append(rows, newEvent)

		// Get the current file's directory
		relativePath := fmt.Sprintf("../db/internal/data/%s.json", node.ID)
		absolutePath := utils.GetPath(relativePath)

		file, err := os.Create(absolutePath)

		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		// Write the updated data back to the file
		if err := json.NewEncoder(file).Encode(rows); err != nil {
			t.Fatalf("Failed to encode JSON: %v", err)
		}

		file.Close()
	}

	time.Sleep(5 * time.Second)

	// Step 2: Run the system
	_, servers, _, cancelFuncs := runSystem(nodes, nodeRings, dataHandlers, gossipFanOut, suspectToDeadTimeout, gossipInterval)

	time.Sleep(10 * time.Second)

	log.Printf("Updated Ring view: %s\n", nodeRings[0].String())

	// Check the data files for each node, each data record must be present in at least 2 nodes and at most 3 nodes
	for _, record := range records {
		found := replicationFactor
		for _, node := range nodes {
			// Get the current file's directory
			relativePath := fmt.Sprintf("../db/internal/data/%s.json", node.ID)
			absolutePath := utils.GetPath(relativePath)

			file, err := os.Open(absolutePath)
			if err != nil {
				t.Fatalf("Failed to open file: %v", err)
			}

			byteValue, err := io.ReadAll(file)
			if err != nil {
				t.Fatalf("Failed to read file: %v", err)
			}

			var rows []db.Row
			err = json.Unmarshal(byteValue, &rows)
			if err != nil {
				t.Fatalf("Failed to unmarshal JSON: %v", err)
			}

			file.Close()

			for _, row := range rows {
				if row.PageId == record["pageID"] && row.ComponentId == record["element"] && row.Timestamp == record["timestamp"] {
					found--
					break
				}
			}

		}
		if found > 0 {
			t.Fatalf("Insufficient Replication: %+v", record)
		}
	}

		// Clean up: Stop remaining servers and cancel contexts
		for i, server := range servers {
			if i != 1 { // Skip the already stopped node
				server.Stop()
				(*cancelFuncs[i])()
			}
		}

}

func TestDataRebalancingAfterNodeFailure(t *testing.T) {
	numNodes := 4
	numVirtualNodes := uint64(3)
	replicationFactor := 2
	gossipFanOut := 2
	suspectToDeadTimeout := 8
	gossipInterval := 3

	// Step 1: Create the initial system
	nodes, nodeRings, dataHandlers := createInitialSystem(numNodes, numVirtualNodes, replicationFactor)

	// Records to insert (same as previous test)
	records := []map[string]string{
		{
			"pageID":     "19",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.847637592Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.851746508Z",
			"created_at": "2024-12-07T07:41:19.851749008Z",
			"hashkey":    "13502972256853596262",
		},
		{
			"pageID":     "86",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.663822341Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.665629591Z",
			"created_at": "2024-12-07T07:41:19.665632425Z",
			"hashkey":    "4392469504148276032",
		},
		{
			"pageID":     "42",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.802749175Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.803729092Z",
			"created_at": "2024-12-07T07:41:19.803732425Z",
			"hashkey":    "13154972877196513132",
		},
		{
			"pageID":     "93",
			"element":    "btn1",
			"timestamp":  "2024-12-07T07:41:19.81675455Z",
			"event":      "click",
			"updated_at": "2024-12-07T07:41:19.8174478Z",
			"created_at": "2024-12-07T07:41:19.8174488Z",
			"hashkey":    "14458771382211144428",
		},
	}

	// Step 2: Create data files for each node
	for i, node := range nodes {
		rows := []db.Row{
			{
				PageId:      records[i]["pageID"],
				ComponentId: records[i]["element"],
				Timestamp:   records[i]["timestamp"],
				Event:       records[i]["event"],
				UpdatedAt:   records[i]["updated_at"],
				CreatedAt:   records[i]["created_at"],
				HashKey:     records[i]["hashkey"],
			},
		}

		relativePath := fmt.Sprintf("../db/internal/data/%s.json", node.ID)
		absolutePath := utils.GetPath(relativePath)

		file, err := os.Create(absolutePath)
		if err != nil {
			t.Fatalf("Failed to create file: %v", err)
		}

		if err := json.NewEncoder(file).Encode(rows); err != nil {
			t.Fatalf("Failed to encode JSON: %v", err)
		}

		file.Close()
	}

	// Step 3: Run the system
	gossipHandlers, servers, _, cancelFuncs := runSystem(nodes, nodeRings, dataHandlers, gossipFanOut, suspectToDeadTimeout, gossipInterval)

	// Wait for the system to stabilize
	time.Sleep(10 * time.Second)

	// Step 4: Simulate node failure by stopping node 1
	log.Printf("Stopping node %s\n", nodes[1].Name)
	(*cancelFuncs[1])()
	servers[1].Stop()


	// Remove the node from the gossip handlers of other nodes
	for i, handler := range gossipHandlers {
		if i != 1 {
			handler.Membership.MarkNodeDead(nodes[1].ID, nodeRings[i])
		}
	}

	// Wait for the system to detect the node failure and rebalance data
	time.Sleep(time.Duration(suspectToDeadTimeout+gossipInterval) * time.Second)

	// Step 5: Verify data rebalancing
	// The data previously held by node 1 should now be present in other nodes
	// Since the replication factor is 2, each data item should be present in 2 nodes

	// Map to track data item presence across nodes
	dataPresence := make(map[string]int) // key: data item identifier, value: count of nodes containing it

	// Collect data from all nodes except the failed one
	for i, node := range nodes {
		if i == 1 {
			continue // Skip the failed node
		}

		// Get the current file's directory
		relativePath := fmt.Sprintf("../db/internal/data/%s.json", node.ID)
		absolutePath := utils.GetPath(relativePath)

		file, err := os.Open(absolutePath)
		if err != nil {
			t.Fatalf("Failed to open file: %v", err)
		}

		byteValue, err := io.ReadAll(file)
		if err != nil {
			t.Fatalf("Failed to read file: %v", err)
		}

		var rows []db.Row
		err = json.Unmarshal(byteValue, &rows)
		if err != nil {
			t.Fatalf("Failed to unmarshal JSON: %v", err)
		}

		file.Close()

		// For each row, update the dataPresence map
		for _, row := range rows {
			key := fmt.Sprintf("%s-%s", row.PageId, row.Timestamp)
			dataPresence[key]++
		}
	}

	// Verify that each data item is present in at least replicationFactor nodes
	for key, count := range dataPresence {
		if count < replicationFactor {
			t.Errorf("Data item %s is present in only %d nodes, expected at least %d", key, count, replicationFactor)
		}
	}

	// Clean up: Stop remaining servers and cancel contexts
	for i, server := range servers {
		if i != 1 { // Skip the already stopped node
			(*cancelFuncs[i])()
			server.Stop()
		}
	}
}
