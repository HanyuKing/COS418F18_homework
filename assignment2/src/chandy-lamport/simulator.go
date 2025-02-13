package chandy_lamport

import (
	"log"
	"math/rand"
)

// Max random delay added to packet delivery
const maxDelay = 5

// Simulator is the entry point to the distributed snapshot application.
//
// It is a discrete time simulator, i.e. events that happen at time t + 1 come
// strictly after events that happen at time t. At each time step, the simulator
// examines messages queued up across all the links in the system and decides
// which ones to deliver to the destination.
//
// The simulator is responsible for starting the snapshot process, inducing servers
// to pass tokens to each other, and collecting the snapshot state after the process
// has terminated.
type Simulator struct {
	time           int
	nextSnapshotId int
	servers        map[string]*Server // key = server ID
	logger         *Logger
	// TODO: ADD MORE FIELDS HERE
	allServerSnapCompleted  chan struct{}
	snapCompletedServerSeqs map[int][]string
	snapCompletedServers    map[int]map[string]struct{}
	snapshotMessages        map[int][]*SnapshotMessage
}

func NewSimulator() *Simulator {
	return &Simulator{
		0,
		0,
		make(map[string]*Server),
		NewLogger(),
		make(chan struct{}),
		make(map[int][]string),
		make(map[int]map[string]struct{}),
		make(map[int][]*SnapshotMessage),
	}
}

// Return the receive time of a message after adding a random delay.
// Note: since we only deliver one message to a given server at each time step,
// the message may be received *after* the time step returned in this function.
func (sim *Simulator) GetReceiveTime() int {
	return sim.time + 1 + rand.Intn(5)
}

// Add a server to this simulator with the specified number of starting tokens
func (sim *Simulator) AddServer(id string, tokens int) {
	server := NewServer(id, tokens, sim)
	sim.servers[id] = server
}

// Add a unidirectional link between two servers
func (sim *Simulator) AddForwardLink(src string, dest string) {
	server1, ok1 := sim.servers[src]
	server2, ok2 := sim.servers[dest]
	if !ok1 {
		log.Fatalf("Server %v does not exist\n", src)
	}
	if !ok2 {
		log.Fatalf("Server %v does not exist\n", dest)
	}
	server1.AddOutboundLink(server2)
}

// Run an event in the system
func (sim *Simulator) InjectEvent(event interface{}) {
	switch event := event.(type) {
	case PassTokenEvent:
		src := sim.servers[event.src]
		src.SendTokens(event.tokens, event.dest)
	case SnapshotEvent:
		sim.StartSnapshot(event.serverId)
	default:
		log.Fatal("Error unknown event: ", event)
	}
}

// Advance the simulator time forward by one step, handling all send message events
// that expire at the new time step, if any.
func (sim *Simulator) Tick() {
	sim.time++
	sim.logger.NewEpoch()
	// Note: to ensure deterministic ordering of packet delivery across the servers,
	// we must also iterate through the servers and the links in a deterministic way
	for _, serverId := range getSortedKeys(sim.servers) {
		server := sim.servers[serverId]
		for _, dest := range getSortedKeys(server.outboundLinks) {
			link := server.outboundLinks[dest]
			// Deliver at most one packet per server at each time step to
			// establish total ordering of packet delivery to each server
			if !link.events.Empty() {
				e := link.events.Peek().(SendMessageEvent)
				if e.receiveTime <= sim.time {
					link.events.Pop()
					sim.logger.RecordEvent(
						sim.servers[e.dest],
						ReceivedMessageEvent{e.src, e.dest, e.message})
					sim.servers[e.dest].HandlePacket(e.src, e.message)
					break
				}
			}
		}
	}
}

// Start a new snapshot process at the specified server
func (sim *Simulator) StartSnapshot(serverId string) {
	snapshotId := sim.nextSnapshotId
	sim.nextSnapshotId++
	// sim.logger.RecordEvent(sim.servers[serverId], StartSnapshot{serverId, snapshotId})
	// TODO: IMPLEMENT ME

	// 1. current server start snapshot
	server := sim.servers[serverId]
	server.StartSnapshot(snapshotId)
	server.sim.NotifySnapshotComplete(serverId, snapshotId)

	// 2. when snapshot completed, send a marker message to others
	markerMessage := MarkerMessage{snapshotId}
	server.SendToNeighbors(markerMessage)
}

// Callback for servers to notify the simulator that the snapshot process has
// completed on a particular server
func (sim *Simulator) NotifySnapshotComplete(serverId string, snapshotId int) {
	sim.logger.RecordEvent(sim.servers[serverId], EndSnapshot{serverId, snapshotId})
	// TODO: IMPLEMENT ME

	if sim.snapCompletedServers[snapshotId] == nil {
		sim.snapCompletedServers[snapshotId] = make(map[string]struct{})
	}

	if _, ok := sim.snapCompletedServers[snapshotId][serverId]; ok {
		return
	}

	sim.snapCompletedServers[snapshotId][serverId] = struct{}{}
	sim.snapCompletedServerSeqs[snapshotId] = append(sim.snapCompletedServerSeqs[snapshotId], serverId)

	// sim.logger.PrettyPrint()
	//if debug {
	//	fmt.Printf("NotifySnapshotComplete. snapshotId: %d, serverId: %s\n", snapshotId, serverId)
	//}

	if len(sim.snapCompletedServerSeqs[snapshotId]) == len(sim.servers) {
		// arrived server message(not snapshot)
		for _, serverId := range getSortedKeys(sim.servers) {
			server := sim.servers[serverId]
			for _, msg := range server.tempSnapshotMessages {

				if sim.snapMessageBeforeSend(snapshotId, msg.src, msg.dest) {
					continue
				}

				sim.snapshotMessages[snapshotId] = append(sim.snapshotMessages[snapshotId], msg)
			}
		}
		// in channel message
		for _, serverId := range getSortedKeys(sim.servers) {
			server := sim.servers[serverId]
			for _, dest := range getSortedKeys(server.outboundLinks) {

				link := server.outboundLinks[dest]
				if !link.events.Empty() {
					node := link.events.elements.Front()
					for node != nil {
						evt := node.Value.(SendMessageEvent)
						switch msg := evt.message.(type) {
						case TokenMessage:
							if sim.snapMessageBeforeSend(snapshotId, evt.src, evt.dest) {
								break
							}

							sim.snapshotMessages[snapshotId] = append(sim.snapshotMessages[snapshotId], &SnapshotMessage{
								src:     evt.src,
								dest:    evt.dest,
								message: msg,
							})
						case MarkerMessage:

						}
						node = node.Next()
					}
				}
			}
		}

		sim.snapCompletedServerSeqs[snapshotId] = nil
		sim.allServerSnapCompleted <- struct{}{}
	}
}

// Collect and merge snapshot state from all the servers.
// This function blocks until the snapshot process has completed on all servers.
func (sim *Simulator) CollectSnapshot(snapshotId int) *SnapshotState {
	// TODO: IMPLEMENT ME
	<-sim.allServerSnapCompleted

	snap := SnapshotState{snapshotId, make(map[string]int), make([]*SnapshotMessage, 0)}

	for _, serverId := range getSortedKeys(sim.servers) {
		server := sim.servers[serverId]
		snap.tokens[server.Id] = server.snapshotTokens[snapshotId]
	}

	snap.messages = append(snap.messages, sim.snapshotMessages[snapshotId]...)

	return &snap
}

func (sim *Simulator) snapMessageBeforeSend(snapshotId int, server1 string, server2 string) bool {
	index := 0
	index1 := index
	index2 := index
	for _, serverId := range sim.snapCompletedServerSeqs[snapshotId] {
		if serverId == server1 {
			index1 = index
			index += 1
		}
		if serverId == server2 {
			index2 = index
			index += 1
		}
	}
	return index1 < index2
}
