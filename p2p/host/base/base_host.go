package base

import (
	"context"
	"io"
	"sync"

	"github.com/libp2p/go-libp2p-core/connmgr"
	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/peerstore"
	"github.com/libp2p/go-libp2p-core/protocol"

	"github.com/libp2p/go-eventbus"

	logging "github.com/ipfs/go-log/v2"
	ma "github.com/multiformats/go-multiaddr"
	msmux "github.com/multiformats/go-multistream"
)

var log = logging.Logger("basehost")

type Option func(*baseHost) error

func WithConnMgr(mgr connmgr.ConnManager) Option {
	return func(h *baseHost) error {
		h.connmgr = mgr
		return nil
	}
}

func WithMultistreamMuxer(mux *msmux.MultistreamMuxer) Option {
	return func(h *baseHost) error {
		h.mux = mux
		return nil
	}
}

type baseHost struct {
	network  network.Network
	eventbus event.Bus
	mux      *msmux.MultistreamMuxer
	connmgr  connmgr.ConnManager

	closeOnce sync.Once

	emitters struct {
		evtLocalProtocolsUpdated    event.Emitter
		evtPeerConnectednessChanged event.Emitter
	}
}

var _ host.Host = &baseHost{}

func NewHost(n network.Network, opts ...Option) (host.Host, error) {
	h := &baseHost{
		network:  n,
		eventbus: eventbus.NewBus(),
	}
	for _, opt := range opts {
		if err := opt(h); err != nil {
			return nil, err
		}
	}
	if h.mux == nil {
		h.mux = msmux.NewMultistreamMuxer()
	}
	if h.connmgr == nil {
		h.connmgr = &connmgr.NullConnMgr{}
	} else {
		n.Notify(h.connmgr.Notifee())
	}

	var err error
	if h.emitters.evtLocalProtocolsUpdated, err = h.eventbus.Emitter(&event.EvtLocalProtocolsUpdated{}); err != nil {
		return nil, err
	}
	h.emitters.evtPeerConnectednessChanged, err = h.eventbus.Emitter(&event.EvtPeerConnectednessChanged{})
	if err != nil {
		return nil, err
	}
	n.Notify(newPeerConnectWatcher(h.emitters.evtPeerConnectednessChanged))

	n.SetStreamHandler(h.newStreamHandler)
	return h, nil
}

func (h *baseHost) Addrs() []ma.Multiaddr {
	addrs, err := h.Network().InterfaceListenAddresses()
	if err != nil {
		log.Debug("error retrieving network interface addrs: ", err)
		return nil
	}
	return addrs
}

func (h *baseHost) Connect(ctx context.Context, ai peer.AddrInfo) error {
	// absorb addresses into peerstore
	h.Peerstore().AddAddrs(ai.ID, ai.Addrs, peerstore.TempAddrTTL)

	cs := h.Network().ConnsToPeer(ai.ID)
	if len(cs) > 0 {
		return nil
	}

	_, err := h.Network().DialPeer(ctx, ai.ID)
	return err
}

func (h *baseHost) ConnManager() connmgr.ConnManager {
	return h.connmgr
}

func (h *baseHost) Close() error {
	h.closeOnce.Do(func() {
		h.Network().Close()
		h.connmgr.Close()
		h.emitters.evtLocalProtocolsUpdated.Close()
		h.emitters.evtPeerConnectednessChanged.Close()

		if h.Peerstore() != nil {
			h.Peerstore().Close()
		}
	})
	return nil
}

func (h *baseHost) EventBus() event.Bus {
	return h.eventbus
}

func (h *baseHost) Peerstore() peerstore.Peerstore {
	return h.Network().Peerstore()
}

// Network returns the Network interface of the Host
func (h *baseHost) Network() network.Network {
	return h.network
}

// Mux returns the Mux multiplexing incoming streams to protocol handlers
func (h *baseHost) Mux() protocol.Switch {
	return h.mux
}

func (h *baseHost) ID() peer.ID {
	return h.Network().LocalPeer()
}

// newStreamHandler is the remote-opened stream handler for network.Network
func (h *baseHost) newStreamHandler(s network.Stream) {
	protoID, handle, err := h.Mux().Negotiate(s)
	if err != nil {
		log.Infow("protocol negotiation failed", "error", err)
		s.Reset()
		return
	}

	s.SetProtocol(protocol.ID(protoID))
	go handle(protoID, s)
}

func (h *baseHost) NewStream(ctx context.Context, p peer.ID, protos ...protocol.ID) (network.Stream, error) {
	s, err := h.Network().NewStream(ctx, p)
	if err != nil {
		return nil, err
	}

	selected, err := msmux.SelectOneOf(protocol.ConvertToStrings(protos), s)
	if err != nil {
		s.Reset()
		return nil, err
	}

	selpid := protocol.ID(selected)
	s.SetProtocol(selpid)
	h.Peerstore().AddProtocols(p, selected)

	return s, nil
}

// SetStreamHandler sets the protocol handler on the Host's Mux.
// This is equivalent to:
//   host.Mux().SetHandler(proto, handler)
// (Threadsafe)
func (h *baseHost) SetStreamHandler(pid protocol.ID, handler network.StreamHandler) {
	h.Mux().AddHandler(string(pid), func(p string, rwc io.ReadWriteCloser) error {
		is := rwc.(network.Stream)
		is.SetProtocol(protocol.ID(p))
		handler(is)
		return nil
	})
	h.emitters.evtLocalProtocolsUpdated.Emit(event.EvtLocalProtocolsUpdated{
		Added: []protocol.ID{pid},
	})
}

// SetStreamHandlerMatch sets the protocol handler on the Host's Mux
// using a matching function to do protocol comparisons
func (h *baseHost) SetStreamHandlerMatch(pid protocol.ID, m func(string) bool, handler network.StreamHandler) {
	h.Mux().AddHandlerWithFunc(string(pid), m, func(p string, rwc io.ReadWriteCloser) error {
		is := rwc.(network.Stream)
		is.SetProtocol(protocol.ID(p))
		handler(is)
		return nil
	})
	h.emitters.evtLocalProtocolsUpdated.Emit(event.EvtLocalProtocolsUpdated{
		Added: []protocol.ID{pid},
	})
}

// RemoveStreamHandler returns ..
func (h *baseHost) RemoveStreamHandler(pid protocol.ID) {
	h.Mux().RemoveHandler(string(pid))
	h.emitters.evtLocalProtocolsUpdated.Emit(event.EvtLocalProtocolsUpdated{
		Removed: []protocol.ID{pid},
	})
}
