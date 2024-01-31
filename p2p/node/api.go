package node

import (
	"math/big"
	"time"

	"github.com/pkg/errors"
	"github.com/spf13/viper"

	"github.com/dominant-strategies/go-quai/cmd/utils"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/log"
	"github.com/dominant-strategies/go-quai/p2p"
	quaiprotocol "github.com/dominant-strategies/go-quai/p2p/protocol"
	"github.com/dominant-strategies/go-quai/quai"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"

	"github.com/dominant-strategies/go-quai/common"
)

// Starts the node and all of its services
func (p *P2PNode) Start() error {
	log.Global.Infof("starting P2P node...")

	// Start any async processes belonging to this node
	log.Global.Debugf("starting node processes...")
	go p.eventLoop()
	go p.statsLoop()

	// Is this node expected to have bootstrap peers to dial?
	if !viper.GetBool(utils.BootNodeFlag.Name) && !viper.GetBool(utils.SoloFlag.Name) && len(p.bootpeers) == 0 {
		err := errors.New("no bootpeers provided. Unable to join network")
		log.Global.Errorf("%s", err)
		return err
	}

	// Register the Quai protocol handler
	p.SetStreamHandler(quaiprotocol.ProtocolVersion, func(s network.Stream) {
		quaiprotocol.QuaiProtocolHandler(s, p)
	})

	// If the node is a bootnode, start the bootnode service
	if viper.GetBool(utils.BootNodeFlag.Name) {
		log.Global.Infof("starting node as a bootnode...")
		return nil
	}

	// Start the pubsub manager
	p.pubsub.Start(p.handleBroadcast)

	return nil
}

func (p *P2PNode) Subscribe(location common.Location, datatype interface{}) error {
	return p.pubsub.Subscribe(location, datatype)
}

func (p *P2PNode) Broadcast(location common.Location, data interface{}) error {
	return p.pubsub.Broadcast(location, data)
}

func (p *P2PNode) SetConsensusBackend(be quai.ConsensusAPI) {
	p.consensus = be
}

type stopFunc func() error

// Function to gracefully shtudown all running services
func (p *P2PNode) Stop() error {
	// define a list of functions to stop the services the node is running
	stopFuncs := []stopFunc{
		p.Host.Close,
		p.dht.Close,
	}
	// create a channel to collect errors
	errs := make(chan error, len(stopFuncs))
	// run each stop function in a goroutine
	for _, fn := range stopFuncs {
		go func(fn stopFunc) {
			errs <- fn()
		}(fn)
	}

	var allErrors []error
	for i := 0; i < len(stopFuncs); i++ {
		select {
		case err := <-errs:
			if err != nil {
				log.Global.Errorf("error during shutdown: %s", err)
				allErrors = append(allErrors, err)
			}
		case <-time.After(5 * time.Second):
			err := errors.New("timeout during shutdown")
			log.Global.Warnf("error: %s", err)
			allErrors = append(allErrors, err)
		}
	}
	close(errs)
	if len(allErrors) > 0 {
		return errors.Errorf("errors during shutdown: %v", allErrors)
	} else {
		return nil
	}
}

func (p *P2PNode) RequestByNumber(location common.Location, number *big.Int, datatype interface{}) chan interface{} {
	resultChan := make(chan interface{}, 1)
	go func() {
		defer close(resultChan)
		// 2. Query the topic peers for the data
		peers, err := p.pubsub.PeersForTopic(location, datatype)
		if err != nil {
			log.Global.Error("Error requesting data: ", err)
			return
		}
		for _, peerID := range peers {
			go func(peerID p2p.PeerID) {
				if recvd, err := p.requestFromPeer(peerID, location, number, datatype); err == nil {
					log.Global.Debugf("Received %s from peer %s", number, peerID)
					// send the block to the result channel
					resultChan <- recvd
				}
			}(peerID)
		}

		// 3. If hash is not found, query the DHT for peers in the slice
		// TODO: evaluate making this configurable
		const (
			maxDHTQueryRetries    = 3  // Maximum number of retries for DHT queries
			peersPerDHTQuery      = 10 // Number of peers to query per DHT attempt
			dhtQueryRetryInterval = 5  // Time to wait between DHT query retries
		)
		// create a Cid from the slice location
		shardCid := locationToCid(location)
		for retries := 0; retries < maxDHTQueryRetries; retries++ {
			log.Global.Debugf("Querying DHT for slice Cid %s (retry %d)", shardCid, retries)
			// query the DHT for peers in the slice
			// TODO: need to find providers of a topic, not a shard
			for peer := range p.dht.FindProvidersAsync(p.ctx, shardCid, peersPerDHTQuery) {
				go func() {
					// Ask peer and wait for response
					if recvd, err := p.requestFromPeer(peer.ID, location, number, datatype); err == nil {
						log.Global.Debugf("Received %s from peer %s", number, peer.ID)
						// send the block to the result channel
						resultChan <- recvd
						// TODO: make sure gossipsub holds onto this good peer for future queries
					}
				}()
			}
			// if the data is not found, wait for a bit and try again
			log.Global.Debugf("Block %s not found in slice %s. Retrying...", number, location)
			time.Sleep(dhtQueryRetryInterval * time.Second)
		}
		log.Global.Debugf("Block %s not found in slice %s", number, location)
	}()
	return resultChan
}

func (p *P2PNode) RequestByHash(location common.Location, hash common.Hash, datatype interface{}) chan interface{} {
	resultChan := make(chan interface{}, 1)
	go func() {
		defer close(resultChan)
		// 1. Check if the data is in the local cache
		if res, ok := p.cacheGet(hash, datatype); ok {
			log.Global.Debugf("data %s found in cache", hash)
			resultChan <- res.(*types.Block)
			return
		}

		// 2. If not, query the topic peers for the data
		peers, err := p.pubsub.PeersForTopic(location, datatype)
		if err != nil {
			log.Global.Error("Error requesting data: ", err)
			return
		}
		for _, peerID := range peers {
			go func(peerID p2p.PeerID) {
				if recvd, err := p.requestFromPeer(peerID, location, hash, datatype); err == nil {
					log.Global.Debugf("Received %s from peer %s", hash, peerID)
					// cache the response
					p.cacheAdd(hash, recvd)
					// send the block to the result channel
					resultChan <- recvd
				}
			}(peerID)
		}

		// 3. If block is not found, query the DHT for peers in the slice
		// TODO: evaluate making this configurable
		const (
			maxDHTQueryRetries    = 3  // Maximum number of retries for DHT queries
			peersPerDHTQuery      = 10 // Number of peers to query per DHT attempt
			dhtQueryRetryInterval = 5  // Time to wait between DHT query retries
		)
		// create a Cid from the slice location
		shardCid := locationToCid(location)
		for retries := 0; retries < maxDHTQueryRetries; retries++ {
			log.Global.Debugf("Querying DHT for slice Cid %s (retry %d)", shardCid, retries)
			// query the DHT for peers in the slice
			// TODO: need to find providers of a topic, not a shard
			for peer := range p.dht.FindProvidersAsync(p.ctx, shardCid, peersPerDHTQuery) {
				go func() {
					// Ask peer and wait for response
					if recvd, err := p.requestFromPeer(peer.ID, location, hash, datatype); err == nil {
						log.Global.Debugf("Received %s from peer %s", hash, peer.ID)
						// cache the response
						p.cacheAdd(hash, recvd)
						// send the block to the result channel
						resultChan <- recvd
						// TODO: make sure gossipsub holds onto this good peer for future queries
					}
				}()
			}
			// if the data is not found, wait for a bit and try again
			log.Global.Debugf("Block %s not found in slice %s. Retrying...", hash, location)
			time.Sleep(dhtQueryRetryInterval * time.Second)
		}
		log.Global.Debugf("Block %s not found in slice %s", hash, location)
	}()
	return resultChan
}

// Request a block from the network for the specified slice
func (p *P2PNode) Request(location common.Location, data interface{}, datatype interface{}) chan interface{} {
	switch data.(type) {
	case common.Hash:
		return p.RequestByHash(location, data.(common.Hash), datatype)
	case *big.Int:
		return p.RequestByNumber(location, data.(*big.Int), datatype)
	}
	log.Global.Fatalf("unsupported request input data field type: %T", data)
	return nil
}

func (p *P2PNode) MarkLivelyPeer(peer p2p.PeerID) {
	log.Global.WithFields(log.Fields{
		"peer": peer,
	}).Debug("Recording well-behaving peer")

	p.peerManager.MarkLivelyPeer(peer)
}

func (p *P2PNode) MarkLatentPeer(peer p2p.PeerID) {
	log.Global.WithFields(log.Fields{
		"peer": peer,
	}).Debug("Recording misbehaving peer")

	p.peerManager.MarkLatentPeer(peer)
}

func (p *P2PNode) ProtectPeer(peer p2p.PeerID) {
	log.Global.WithFields(log.Fields{
		"peer": peer,
	}).Debug("Protecting peer connection from pruning")

	p.peerManager.ProtectPeer(peer)
}

func (p *P2PNode) BanPeer(peer p2p.PeerID) {
	log.Global.WithFields(log.Fields{
		"peer": peer,
	}).Warn("Banning peer for misbehaving")

	p.peerManager.BanPeer(peer)
	p.Host.Network().ClosePeer(peer)
}

// Returns the list of bootpeers
func (p *P2PNode) GetBootPeers() []peer.AddrInfo {
	return p.bootpeers
}

// Opens a new stream to the given peer using the given protocol ID
func (p *P2PNode) NewStream(peerID peer.ID, protocolID protocol.ID) (network.Stream, error) {
	return p.Host.NewStream(p.ctx, peerID, protocolID)
}

// Connects to the given peer
func (p *P2PNode) Connect(pi peer.AddrInfo) error {
	return p.Host.Connect(p.ctx, pi)
}

// Search for a block in the node's cache, or query the consensus backend if it's not found in cache.
// Returns nil if the block is not found.
func (p *P2PNode) GetBlock(hash common.Hash, location common.Location) *types.Block {
	if res, ok := p.cacheGet(hash, &types.Block{}); ok {
		return res.(*types.Block)
	} else {
		return p.consensus.LookupBlock(hash, location)
	}
}

func (p *P2PNode) GetBlockHashByNumber(number *big.Int, location common.Location) *common.Hash {
	return p.consensus.LookupBlockHashByNumber(number, location)
}

func (p *P2PNode) GetHeader(hash common.Hash, location common.Location) *types.Header {
	panic("TODO: implement")
}

func (p *P2PNode) handleBroadcast(sourcePeer peer.ID, data interface{}, nodeLocation common.Location) {
	switch v := data.(type) {
	case types.Block:
		p.cacheAdd(v.Hash(), &v)
	// TODO: send it to consensus
	default:
		log.Global.Debugf("received unsupported block broadcast")
		// TODO: ban the peer which sent it?
		return
	}

	// If we made it here, pass the data on to the consensus backend
	if p.consensus != nil {
		p.consensus.OnNewBroadcast(sourcePeer, data, nodeLocation)
	}
}
