package main

import (
	"context"
	"eth2-lurk/gossip"
	"eth2-lurk/node"
	"eth2-lurk/peering/discv5"
	"eth2-lurk/peering/kad"
	"eth2-lurk/peering/static"
	"fmt"
	"github.com/libp2p/go-libp2p-core/peer"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/sirupsen/logrus"
	"os"
	"os/signal"
	"path"
	"syscall"
	"time"
)

func main() {
	topics := map[string]string{
		"blocks":             "/eth2/beacon_block/ssz",
		"aggregate":          "/eth2/beacon_aggregate_and_proof/ssz",
		"legacy_attestation": "/eth2/beacon_attestation/ssz",
		"voluntary_exit":     "/eth2/voluntary_exit/ssz",
		"proposer_slashing":  "/eth2/proposer_slashing/ssz",
		"attester_slashing":  "/eth2/attester_slashing/ssz",
		// TODO make this configurable
	}
	for i := 0; i < 8; i++ {
		topics[fmt.Sprintf("committee_%d", i)] = fmt.Sprintf("/eth2/committee_index%d_beacon_attestation/ssz", i)
	}

	ctx, cancel := context.WithCancel(context.Background())

	log := logrus.New()
	log.SetFormatter(&logrus.TextFormatter{})
	log.SetOutput(os.Stdout)
	log.SetLevel(logrus.TraceLevel)

	lu, err := NewLurker(ctx, log)
	if err != nil {
		panic(err)
	}

	outPath := "data"

	lurkAndLog := func(ctx context.Context, outName string, topic string) error {
		topicLog := log.WithField("log_topic", "topic_lurker")
		out, err := os.OpenFile(path.Join(outPath, outName+".txt"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, os.ModePerm)
		if err != nil {
			return err
		}
		go func() {
			ticker := time.NewTicker(time.Second * 60)
			for {
				select {
				case <-ticker.C:
					if err := out.Sync(); err != nil {
						topicLog.Errorf("Synced %s storage with error: %v", outName, err)
					}
				case <-ctx.Done():
					if err := out.Close(); err != nil {
						topicLog.Errorf("Closed %s storage with error: %v", outName, err)
					}
					return
				}
			}
		}()
		errLogger := gossip.NewErrLoggerChannel(ctx, topicLog, outName)
		msgLogger := gossip.NewMessageLogger(ctx, out, errLogger)
		return lu.GS.LurkTopic(ctx, topic, msgLogger, errLogger)
	}

	for name, top := range topics {
		if err := lurkAndLog(ctx, name, top); err != nil {
			panic(fmt.Errorf("topic %s failed to start running: %v", name, err))
		}
	}

	// Connect with peers after the pubsub is all set up,
	// so that peers do not have to learn about pubsub interest after being connected.

	// kademlia
	bootAddrStrs, err := static.ParseMultiAddrs("/dns4/prylabs.net/tcp/30001/p2p/16Uiu2HAm7Qwe19vz9WzD2Mxn7fXd1vgHHp4iccuyq7TxwRXoAGfc")
	if err != nil {
		panic(err)
	}

	if err := lu.ConnectBootNodes(bootAddrStrs); err != nil {
		panic(err)
	}

	// disc v5
	//dv5Nodes, err := discv5.ParseDiscv5ENRs(....)
	//if err != nil {
	//	panic(err)
	//}
	//if err := lu.Dv5.AddDiscV5BootNodes(dv5Nodes); err != nil {
	//	panic(err)
	//}


	// static peers
	staticAddrs, err := static.ParseMultiAddrs() // TODO any static addrs to connect to?
	if err != nil {
		panic(err)
	}
	if err := lu.ConnectStaticPeers(staticAddrs); err != nil {
		panic(err)
	}

	lu.peerInfoLoop(ctx, log.WithField("log_topic", "peer_info"))

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT)

	select {
	case <-stop:
		log.Info("Exiting...")
		cancel()
		time.Sleep(time.Second)
		os.Exit(0)
	}
}

type Lurker struct {
	ctx context.Context
	close func()

	Node node.Node
	Kad kad.Kademlia
	Dv5 discv5.Discv5
	GS gossip.GossipSub
}

func NewLurker(ctx context.Context, log logrus.FieldLogger) (*Lurker, error) {
	ctx, cancel := context.WithCancel(ctx)
	n, err := node.NewLocalNode(ctx, log)
	if err != nil {
		return nil, err
	}
	k, err := kad.NewKademlia(ctx, n, "/prysm/0.0.0/dht")
	if err != nil {
		return nil, err
	}
	gs, err := gossip.NewGossipSub(ctx, n)
	if err != nil {
		return nil, err
	}
	lu := &Lurker{
		ctx: ctx,
		close: cancel,
		Node: n,
		Kad: k,
		//Dv5: discv5.NewDiscV5(ctx, n, dv5Addr, privKey), // TODO: setup discv5
		GS: gs,
	}

	return lu, nil
}

func (lu *Lurker) peerInfoLoop(ctx context.Context, log logrus.FieldLogger) {
	go func() {
		ticker := time.NewTicker(time.Second * 60)
		end := ctx.Done()
		peerstore := lu.Node.Host().Peerstore()
		net := lu.Node.Host().Network()
		strAddrs := func(peerID peer.ID) string {
			out := ""
			for _, a := range peerstore.Addrs(peerID) {
				out += " "
				out += a.String()
			}
			return out
		}
		for {
			select {
			case <-ticker.C:
				peers := peerstore.Peers()
				log.Info("Peerstore size: %d", peers.Len())
				for i, peerID := range peers {
					log.Debugf(" %d id: %x  %s", i, peerID, strAddrs(peerID))
				}
				connPeers := net.Peers()
				log.Info("Network peers size: %d", len(connPeers))
				for i, peerID := range connPeers {
					log.Debugf(" %d id: %x", i, peerID)
				}
				conns := net.Conns()
				log.Info("Connections count: %d", len(conns))
				for i, conn := range conns {
					log.Debugf(" %d id: %x  %s", i, conn.RemotePeer(), conn.RemoteMultiaddr())
				}
			case <-end:
				log.Info("stopped logging peer info")
				return
			}
		}
	}()
}

func (lu *Lurker) ConnectStaticPeers(multiAddrs []ma.Multiaddr) error {
	return static.ConnectStaticPeers(lu.ctx, lu.Node, multiAddrs, nil)
}

func (lu *Lurker) ConnectBootNodes(bootAddrs []ma.Multiaddr) error {
	return static.ConnectBootNodes(lu.ctx, lu.Node, bootAddrs)
}

// TODO: implement discv5 polling routine to onboard new peers from into libp2p

func (lu *Lurker) StartWatchingPeers() {
	// TODO: log peers
}

// TODO: re-establish connections with disconnected peers

// TODO: prune peers

// TODO: log metrics of connected/disconnected/etc.

