package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/ethereum/go-ethereum/core/forkid"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/p2p/enr"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/rlp"
)

var (
	verbosity = flag.Int("verbosity", 3, "logging verbosity")
	port      = flag.Int("port", 8888, "port to listen on")
)

var (
	validETHEntry   = mockETHEntry{ForkID: forkid.ID{Hash: [4]byte{1, 2, 3, 4}}}
	invalidETHEntry = mockETHEntry{ForkID: forkid.ID{Hash: [4]byte{5, 6, 7, 8}}}
)

func main() {
	flag.Parse()

	log.Root().SetHandler(log.LvlFilterHandler(log.Lvl(*verbosity), log.StreamHandler(os.Stdout, log.TerminalFormat(false))))

	services := map[string]adapters.LifecycleConstructor{
		"valid": func(ctx *adapters.ServiceContext, stack *node.Node) (node.Lifecycle, error) {
			s := newMockService("valid")
			s.SetAttributes([]enr.Entry{validETHEntry})
			stack.RegisterProtocols(s.Protocols())
			return s, nil
		},
		"invalid": func(ctx *adapters.ServiceContext, stack *node.Node) (node.Lifecycle, error) {
			s := newMockService("invalid")
			s.SetAttributes([]enr.Entry{invalidETHEntry})
			stack.RegisterProtocols(s.Protocols())
			return s, nil
		},
	}
	adapters.RegisterLifecycles(services)

	adapter := adapters.NewSimAdapter(services)

	log.Info("starting simulation server", "port", *port)
	network := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		DefaultService: "valid",
	})
	if err := http.ListenAndServe(fmt.Sprintf(":%d", *port), simulations.NewServer(network)); err != nil {
		log.Crit("error starting simulation server", "err", err)
	}
}

type mockService struct {
	name   string
	attrs  []enr.Entry
	ctx    context.Context
	cancel context.CancelFunc
}

func newMockService(name string) *mockService {
	s := &mockService{
		name: name,
	}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	return s
}

func (s *mockService) SetAttributes(attrs []enr.Entry) {
	s.attrs = attrs
}

func (s *mockService) Protocols() []p2p.Protocol {
	return []p2p.Protocol{{
		Name:       s.name,
		Version:    1,
		Length:     1,
		Run:        s.Run,
		NodeInfo:   s.Info,
		Attributes: s.attrs,
	}}
}

func (s *mockService) Start() error {
	return nil
}

func (s *mockService) Stop() error {
	s.cancel()
	return nil
}

func (s *mockService) Info() interface{} {
	return nil
}

func (s *mockService) Run(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
	if !peer.RunningCap(s.name, []uint{1}) {
		log.Error("peer does not support protocol", "peer", peer.ID())
		return fmt.Errorf("peer does not support protocol %s", s.name)
	}

	<-s.ctx.Done()
	return nil
}

type mockETHEntry struct {
	ForkID forkid.ID
	Rest   []rlp.RawValue `rlp:"tail"`
}

func (e mockETHEntry) ENRKey() string {
	return "eth"
}