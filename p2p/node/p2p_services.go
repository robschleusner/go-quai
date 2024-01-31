package node

import (
	"errors"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multihash"

	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/log"
	"github.com/dominant-strategies/go-quai/p2p/pb"
	"github.com/dominant-strategies/go-quai/p2p/protocol"
)

// Opens a stream to the given peer and request some data for the given hash at the given location
func (p *P2PNode) requestFromPeer(peerID peer.ID, location common.Location, data interface{}, datatype interface{}) (interface{}, error) {
	stream, err := p.NewStream(peerID, protocol.ProtocolVersion)
	if err != nil {
		// TODO: should we report this peer for failure to participate?
		return nil, err
	}
	defer stream.Close()

	// Get a new request ID
	id := protocol.GetRequestIDManager().GenerateRequestID()

	// Create the corresponding data request
	requestBytes, err := pb.EncodeQuaiRequest(id, location, data, datatype)
	if err != nil {
		return nil, err
	}

	// Send the request to the peer
	err = common.WriteMessageToStream(stream, requestBytes)
	if err != nil {
		return nil, err
	}

	// Add request ID to the map of pending requests
	protocol.GetRequestIDManager().AddRequestID(id)

	// Read the response from the peer
	responseBytes, err := common.ReadMessageFromStream(stream)
	if err != nil {
		return nil, err
	}

	// Unmarshal the response
	recvdID, recvdType, err := pb.DecodeQuaiResponse(responseBytes)
	if err != nil {
		// TODO: should we report this peer for an invalid response?
		return nil, err
	}

	// Check the received request ID matches the request
	if recvdID != id {
		log.Global.Warn("peer returned unexpected request ID")
		panic("TODO: implement")
	}

	// Remove request ID from the map of pending requests
	protocol.GetRequestIDManager().RemoveRequestID(id)

	// Check the received data type & hash matches the request
	switch datatype.(type) {
	case *types.Block:
		if block, ok := recvdType.(*types.Block); ok && block.Hash() == data.(common.Hash) {
			return block, nil
		}
	case *types.Header:
		if header, ok := recvdType.(*types.Header); ok && header.Hash() == data.(common.Hash) {
			return header, nil
		}
	case *types.Transaction:
		if tx, ok := recvdType.(*types.Transaction); ok && tx.Hash() == data.(common.Hash) {
			return tx, nil
		}
	case *common.Hash:
		// TODO: Check that block hash matches the request number
		if hash, ok := recvdType.(*common.Hash); ok {
			return hash, nil
		}
	default:
		log.Global.Warn("peer returned unexpected type")
	}

	// If this peer responded with an invalid response, ban them for misbehaving.
	p.BanPeer(peerID)
	return nil, errors.New("invalid response")
}

// Creates a Cid from a location to be used as DHT key
func locationToCid(location common.Location) cid.Cid {
	sliceBytes := []byte(location.Name())

	// create a multihash from the slice ID
	mhash, _ := multihash.Encode(sliceBytes, multihash.SHA2_256)

	// create a Cid from the multihash
	return cid.NewCidV1(cid.Raw, mhash)

}
