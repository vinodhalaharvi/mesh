package metcd

import (
	"fmt"
	"log"
	"net"
	"os"
	"time"

	wackygrpc "github.com/coreos/etcd/Godeps/_workspace/src/google.golang.org/grpc"
	"github.com/coreos/etcd/raft/raftpb"

	"github.com/weaveworks/mesh"
	"github.com/weaveworks/mesh/meshconn"
)

// NewServer returns a gRPC server that implements the etcd V3 API.
// It uses the passed mesh components to create and manage the Raft transport.
// For the moment, it blocks until the mesh has minPeerCount peers.
// (This responsibility should instead be given to the caller.)
func NewServer(
	router *mesh.Router,
	peer *meshconn.Peer,
	minPeerCount int,
	logger *log.Logger,
) *wackygrpc.Server {
	c := make(chan *wackygrpc.Server)
	go grpcManager(router, peer, minPeerCount, logger, c)
	return <-c
}

func grpcManager(
	router *mesh.Router,
	peer *meshconn.Peer,
	minPeerCount int,
	logger *log.Logger,
	out chan<- *wackygrpc.Server,
) {
	// Identify mesh peers to either create or join a cluster.
	// This algorithm is presently completely insufficient.
	// It suffers from timing failures, and doesn't understand channels.
	// TODO(pb): use gossip to agree on better starting conditions
	var (
		self   = meshconn.MeshAddr{PeerName: router.Ourself.Peer.Name, PeerUID: router.Ourself.UID}
		others = []net.Addr{}
	)
	for {
		others = others[:0]
		for _, desc := range router.Peers.Descriptions() {
			others = append(others, meshconn.MeshAddr{PeerName: desc.Name, PeerUID: desc.UID})
		}
		if len(others) == minPeerCount {
			logger.Printf("detected %d peers; creating", len(others))
			break
		} else if len(others) > minPeerCount {
			logger.Printf("detected %d peers; joining", len(others))
			others = others[:0] // empty others slice means join
			break
		}
		logger.Printf("detected %d peers; waiting...", len(others))
		time.Sleep(time.Second)
	}

	var (
		incomingc    = make(chan raftpb.Message)    // from meshconn to ctrl
		outgoingc    = make(chan raftpb.Message)    // from ctrl to meshconn
		unreachablec = make(chan uint64, 10000)     // from meshconn to ctrl
		confchangec  = make(chan raftpb.ConfChange) // from meshconn to ctrl
		snapshotc    = make(chan raftpb.Snapshot)   // from ctrl to state machine
		entryc       = make(chan raftpb.Entry)      // from ctrl to state
		confentryc   = make(chan raftpb.Entry)      // from state to configurator
		proposalc    = make(chan []byte)            // from state machine to ctrl
		removedc     = make(chan struct{})          // from ctrl to us
		shrunkc      = make(chan struct{})          // from membership to us
	)

	// Create the thing that watches the cluster membership via the router. It
	// signals conf changes, and closes shrunkc when the cluster is too small.
	var (
		addc = make(chan uint64)
		remc = make(chan uint64)
	)
	m := newMembership(router, membershipSet(router), minPeerCount, addc, remc, shrunkc, logger)
	defer m.stop()

	// Create the thing that converts mesh membership changes to Raft ConfChange
	// proposals.
	c := newConfigurator(addc, remc, confchangec, confentryc, logger)
	defer c.stop()

	// Create a packet transport, wrapping the meshconn.Peer.
	transport := newPacketTransport(peer, translateVia(router), incomingc, outgoingc, unreachablec, logger)
	defer transport.stop()

	// Create the API server. store.stop must go on the defer stack before
	// ctrl.stop so that the ctrl stops first. Otherwise, ctrl can deadlock
	// processing the last tick.
	store := newEtcdStore(proposalc, snapshotc, entryc, confentryc, logger)
	defer store.stop()

	// Create the controller, which drives the Raft node internally.
	ctrl := newCtrl(self, others, minPeerCount, incomingc, outgoingc, unreachablec, confchangec, snapshotc, entryc, proposalc, removedc, logger)
	defer ctrl.stop()

	// Create the gRPC server, wrapping the store. This is what gets returned to
	// the user. But, we can shut it down in certain circumstances.
	server := grpcServer(store)
	defer server.Stop()
	out <- server

	errc := make(chan error)
	go func() {
		<-removedc
		errc <- fmt.Errorf("the Raft peer was removed from the cluster")
	}()
	go func() {
		<-shrunkc
		errc <- fmt.Errorf("the Raft cluster got too small")
	}()
	logger.Print(<-errc)
}

// NewDefaultServer is like NewServer, but we take care of creating a mesh.Router
// and meshconn.Peer for you, using sane defaults. If you need more fine-grained
// control, create these components yourself and use NewServer.
func NewDefaultServer(minPeerCount int, logger *log.Logger) *wackygrpc.Server {
	var (
		peerName = mustPeerName()
		nickName = mustHostname()
		host     = "0.0.0.0"
		port     = 6379
		password = ""
		channel  = "metcd"
	)
	router := mesh.NewRouter(mesh.Config{
		Host:               host,
		Port:               port,
		ProtocolMinVersion: mesh.ProtocolMinVersion,
		Password:           []byte(password),
		ConnLimit:          64,
		PeerDiscovery:      true,
		TrustedSubnets:     []*net.IPNet{},
	}, peerName, nickName, mesh.NullOverlay{}, logger)

	// Create a meshconn.Peer and connect it to a channel.
	peer := meshconn.NewPeer(router.Ourself.Peer.Name, router.Ourself.UID, logger)
	gossip := router.NewGossip(channel, peer)
	peer.Register(gossip)

	// Start the router and join the mesh.
	// Note that we don't ever stop the router.
	// This may or may not be a problem.
	// TODO(pb): determine if this is a super huge problem
	router.Start()

	return NewServer(router, peer, minPeerCount, logger)
}

func translateVia(router *mesh.Router) peerTranslator {
	return func(uid mesh.PeerUID) (mesh.PeerName, error) {
		for _, d := range router.Peers.Descriptions() {
			if d.UID == uid {
				return d.Name, nil
			}
		}
		return 0, fmt.Errorf("peer UID %x not known", uid)
	}
}

func mustPeerName() mesh.PeerName {
	peerName, err := mesh.PeerNameFromString(mustHardwareAddr())
	if err != nil {
		panic(err)
	}
	return peerName
}

func mustHardwareAddr() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		panic(err)
	}
	for _, iface := range ifaces {
		if s := iface.HardwareAddr.String(); s != "" {
			return s
		}
	}
	panic("no valid network interfaces")
}

func mustHostname() string {
	hostname, err := os.Hostname()
	if err != nil {
		panic(err)
	}
	return hostname
}
