package client

import (
	"context"
	"math/rand"
	"sync"
	"time"

	"github.com/p4gefau1t/trojan-go/common"
	"github.com/p4gefau1t/trojan-go/conf"
	"github.com/p4gefau1t/trojan-go/log"
	"github.com/p4gefau1t/trojan-go/protocol"
	"github.com/p4gefau1t/trojan-go/protocol/trojan"
	"github.com/xtaci/smux"
)

type muxID uint32

func generateMuxID() muxID {
	return muxID(rand.Uint32())
}

type muxClientInfo struct {
	id             muxID
	client         *smux.Session
	lastActiveTime time.Time
}

type muxPoolManager struct {
	sync.Mutex
	muxPool map[muxID]*muxClientInfo
	config  *conf.GlobalConfig
	ctx     context.Context
}

func (m *muxPoolManager) newMuxClient() (*muxClientInfo, error) {
	id := generateMuxID()
	if _, found := m.muxPool[id]; found {
		return nil, common.NewError("duplicated id")
	}
	req := &protocol.Request{
		Command: protocol.Mux,
		Address: &common.Address{
			DomainName:  "MUX_CONN",
			AddressType: common.DomainName,
		},
	}
	rwc, err := DialTLSToServer(m.config)
	if err != nil {
		return nil, common.NewError("failed to dail to remote server").Base(err)
	}
	conn, err := trojan.NewOutboundConnSession(req, rwc, m.config)
	if err != nil {
		log.Error(common.NewError("failed to dial tls tunnel").Base(err))
		return nil, err
	}

	client, err := smux.Client(conn, nil)
	common.Must(err)
	log.Info("mux TLS tunnel established, id:", id)
	return &muxClientInfo{
		client:         client,
		id:             id,
		lastActiveTime: time.Now(),
	}, nil
}

func (m *muxPoolManager) pickMuxClient() (*muxClientInfo, error) {
	m.Lock()
	defer m.Unlock()

	for _, info := range m.muxPool {
		if info.client.IsClosed() {
			delete(m.muxPool, info.id)
			log.Info("mux", info.id, "is dead")
			continue
		}
		if info.client.NumStreams() < m.config.Mux.Concurrency || m.config.Mux.Concurrency <= 0 {
			info.lastActiveTime = time.Now()
			return info, nil
		}
	}

	//not found
	info, err := m.newMuxClient()
	if err != nil {
		return nil, err
	}
	m.muxPool[info.id] = info
	return info, nil
}

func (m *muxPoolManager) OpenMuxConn() (*smux.Stream, *muxClientInfo, error) {
	info, err := m.pickMuxClient()
	if err != nil {
		return nil, nil, err
	}
	stream, err := info.client.OpenStream()
	if err != nil {
		m.Lock()
		defer m.Unlock()
		delete(m.muxPool, info.id)
		info.client.Close()
		log.Info("somthing wrong with mux", info.id, ", closing")
		return nil, nil, err
	}
	info.lastActiveTime = time.Now()
	return stream, info, nil
}

func (m *muxPoolManager) checkAndCloseIdleMuxClient() {
	var muxIdleDuration, checkDuration time.Duration
	if m.config.Mux.IdleTimeout <= 0 {
		muxIdleDuration = 0
		checkDuration = time.Second * 10
		log.Warn("invalid mux idle timeout")
	} else {
		muxIdleDuration = time.Duration(m.config.Mux.IdleTimeout) * time.Second
		checkDuration = muxIdleDuration / 4
	}
	for {
		select {
		case <-time.After(checkDuration):
			m.Lock()
			for id, info := range m.muxPool {
				if info.client.IsClosed() {
					delete(m.muxPool, id)
					log.Info("mux", id, "is dead")
				} else if info.client.NumStreams() == 0 && time.Now().Sub(info.lastActiveTime) > muxIdleDuration {
					info.client.Close()
					delete(m.muxPool, id)
					log.Info("mux", id, "is closed due to inactive")
				}
			}
			if len(m.muxPool) != 0 {
				log.Info("current mux pool conn num", len(m.muxPool))
			}
			m.Unlock()
		case <-m.ctx.Done():
			log.Debug("shutting down mux manager..")
			m.Lock()
			for id, info := range m.muxPool {
				info.client.Close()
				log.Info("mux", id, "closed")
			}
			m.Unlock()
			return
		}
	}
}

func NewMuxPoolManager(ctx context.Context, config *conf.GlobalConfig) (*muxPoolManager, error) {
	m := &muxPoolManager{
		ctx:     ctx,
		config:  config,
		muxPool: make(map[muxID]*muxClientInfo),
	}
	go m.checkAndCloseIdleMuxClient()
	return m, nil
}
