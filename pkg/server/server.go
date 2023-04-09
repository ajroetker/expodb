package server

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"

	"github.com/epsniff/expodb/pkg/config"
	"github.com/epsniff/expodb/pkg/server/agents/multiraft"
	serfagent "github.com/epsniff/expodb/pkg/server/agents/serf"
	machines "github.com/epsniff/expodb/pkg/server/state-machines"
	"github.com/hashicorp/serf/serf"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type server struct {
	config *config.Config
	logger *zap.Logger

	metadata *metadata

	serfAgent *serfagent.Agent

	raftAgent raftAgent
}

type raftAgent interface {
	LeaderNotifyCh() <-chan bool
	AddVoter(id, peerAddress string) error
	Apply(val machines.RaftEntry) error
	GetByRowKey(table, key string) (map[string]string, error)
	IsLeader() bool
	//LeaderAddress() string
	Shutdown() error
}

// GetByRowKey gets a value from the raft key value fsm
func (n *server) GetByRowKey(table, key string) (map[string]string, error) {
	val, err := n.raftAgent.GetByRowKey(table, key)
	return val, err
}

func (n *server) GetByRowByQuery(table, query string) ([]map[string]string, error) {
	panic("not implemented")
	// vals, err := n.raftKvpStore.GetByQuery(table, query)
	// return vals, err
}

// SetKeyVal sets a value in the raft key value fsm, if we aren't the
// current leader then forward the request onto the leader node.
func (n *server) SetKeyVal(table, key, col, val string) error {
	//kve := simplestore.NewKeyValEvent(simplestore.UpdateRowOp, table, col, key, val)
	kve := multiraft.KVData{Key: table + ":" + key + ":" + col, Val: val}
	return n.raftAgent.Apply(kve)
}

func New(config *config.Config, logger *zap.Logger) (*server, error) {
	if err := os.MkdirAll(config.SerfDataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to make serf data dir: %w", err)
	}
	serfAgent, err := serfagent.New(config, logger.Named("serf-agent"))
	if err != nil {
		return nil, fmt.Errorf("failed to create serf agent: %w", err)
	}

	if err := os.MkdirAll(config.RaftDataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to make raft data dir: %w", err)
	}
	raftAgent, err := multiraft.New(config, logger.Named("raft-agent"))
	if err != nil {
		return nil, fmt.Errorf("failed to create raft agent: %w", err)
	}

	ser := &server{
		config: config,
		logger: logger,

		metadata: NewMetadata(),

		raftAgent: raftAgent,

		serfAgent: serfAgent,
	}

	// register ourselfs as a handler for serf events. See (n *server) HandleEvent(e serf.Event)
	serfAgent.RegisterEventHandler(ser)

	return ser, nil
}

// HandleEvent is our tap into serf events.  As the Serf(aka gossip) agent detects changes to the cluster
// state we'll handle the events in this handler.
func (n *server) HandleEvent(e serf.Event) {
	switch e.EventType() {
	case serf.EventMemberJoin:
		me := e.(serf.MemberEvent)
		n.logger.Info("Server Serf Handler: Member Join", zap.String("serf-event", fmt.Sprintf("%+v", me.Members)))

		for _, m := range me.Members {
			nodedata, err := n.metadata.Add(m)
			if err != nil {
				n.logger.Error("Error processing metadata",
					zap.String("serf.Member", fmt.Sprintf("%+v", m)), zap.Error(err),
				)
			}
			if !n.raftAgent.IsLeader() {
				n.logger.Info("Not the raft leader, skipping join")
				// We aren't the raft leader nothing else to do but to record the nodes metadata.
				continue
			}

			// join new peer as a raft voter
			err = n.raftAgent.AddVoter(nodedata.Id(), nodedata.RaftAddr())
			if err != nil {
				n.logger.Error("Error joining peer to Raft",
					zap.String("peer.id", nodedata.Id()),
					zap.String("peer.remoteaddr", nodedata.RaftAddr()),
					zap.Error(err),
				)
			}
			n.logger.Info("Peer joined Raft", zap.String("peer.id", nodedata.Id()),
				zap.String("peer.remoteaddr", nodedata.RaftAddr()))
		}
	case serf.EventMemberReap:
		me := e.(serf.MemberEvent)
		n.logger.Info("Server Serf Handler: Member Reap", zap.String("serf-event", fmt.Sprintf("%+v", me)))
	case serf.EventMemberLeave, serf.EventMemberFailed:
		me := e.(serf.MemberEvent)
		n.logger.Info("Server Serf Handler: Member Leave/Failed", zap.String("serf-event", fmt.Sprintf("%+v", me)))
	default:
		n.logger.Info("Server Serf Handler: Unhandled type", zap.String("serf-event", fmt.Sprintf("%+v", e)))
	}
}

// Serve runs the server's agents and blocks until one of the following:
// 1) An agent returns an error
// 2) A Ctrl-C signal is catch.
func (n *server) Serve() error {
	ctx, can := context.WithCancel(context.Background())
	defer can()
	g, ctx := errgroup.WithContext(ctx)

	// Run monitoring leadership
	g.Go(func() error {
		return n.monitorLeadership(ctx)
	})

	// Run HTTP server
	g.Go(func() error {
		// Construct HTTP Address
		httpAddr := &net.TCPAddr{
			IP:   net.ParseIP(n.config.HTTPBindAddress),
			Port: n.config.HTTPBindPort,
		}
		httpServer := &httpServer{
			node:    n,
			address: httpAddr,
			logger:  n.logger.Named("http"),
		}
		go httpServer.Start() // there isn't a wait to use a context to cancel an http server?? so just spin it off in a go routine for now.
		return nil
	})

	// Run serf agent
	g.Go(func() error {
		if err := n.serfAgent.Start(); err != nil {
			can()
			n.logger.Error("serf agent failed to start", zap.Error(err))
			return err
		}
		n.logger.Info("serf agent started", zap.Bool("isSeed", n.config.IsSerfSeed), zap.String("node-name", n.serfAgent.SerfConfig().NodeName))
		if !n.config.IsSerfSeed {
			n.logger.Info("joining serf cluster using", zap.Strings("peers", n.config.SerfJoinAddrs))
			const replay = false
			n.serfAgent.Join(n.config.SerfJoinAddrs, replay)
		}

		<-n.serfAgent.ShutdownCh() // wait for the serf agent to shutdown
		can()
		n.logger.Info("The serf agent shutdown successfully")
		return nil
	})

	// Handler for Ctrl+C and then call n.serfag.Shutdown()
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt)
	g.Go(func() error {
		select {
		case <-ctx.Done():
		case <-signalChan:
			can()
		}
		return nil
	})

	// Go routine to cleanup seft agent on shutdown
	g.Go(func() error {
		<-ctx.Done()
		n.logger.Info("Stopping serf agent")
		return n.serfAgent.Shutdown()
	})

	// Go routine to cleanup raft agent on shutdown
	g.Go(func() error {
		<-ctx.Done()
		n.logger.Info("Stopping raft agent")
		return n.raftAgent.Shutdown()
	})

	n.logger.Info("Server started")
	if err := g.Wait(); err != nil {
		n.logger.Warn("Child workers returned an error")
		return err
	} else {
		n.logger.Info("Clean shutdown")
	}
	return nil
}
