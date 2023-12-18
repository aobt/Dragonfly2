/*
 *     Copyright 2023 The Dragonfly Authors
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package pex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	dfdaemonv1 "d7y.io/api/v2/pkg/apis/dfdaemon/v1"
	logger "d7y.io/dragonfly/v2/internal/dflog"
	"d7y.io/dragonfly/v2/pkg/dfnet"
	"d7y.io/dragonfly/v2/pkg/net/ip"
	dfdaemonclient "d7y.io/dragonfly/v2/pkg/rpc/dfdaemon/client"
)

type peerExchangeMemberManager struct {
	logger          *logger.SugaredLoggerOnWith
	GRPCCredentials credentials.TransportCredentials
	GRPCDialTimeout time.Duration
	peerUpdateChan  <-chan *dfdaemonv1.PeerMetadata

	nodes      sync.Map
	peerPool   *peerPool
	memberPool *memberPool
}

func newPeerExchangeMemberManager(peerUpdateChan <-chan *dfdaemonv1.PeerMetadata) *peerExchangeMemberManager {
	return &peerExchangeMemberManager{
		logger:          logger.With("component", "peerExchangeCluster"),
		GRPCCredentials: nil, // TODO
		GRPCDialTimeout: 0,   // TODO
		peerUpdateChan:  peerUpdateChan,
		nodes:           sync.Map{},
		peerPool:        newPeerPool(),
		memberPool:      newMemberPool(),
	}
}

func (p *peerExchangeMemberManager) NotifyJoin(node *memberlist.Node) {
	addr := node.Addr.String()
	p.logger.Infof("peer %s joined", addr)
	go p.syncNode(node)
}

func (p *peerExchangeMemberManager) NotifyLeave(node *memberlist.Node) {
	addr := node.Addr.String()
	p.logger.Infof("peer %s leaved", addr)
	// TODO
}

func (p *peerExchangeMemberManager) NotifyUpdate(node *memberlist.Node) {
	addr := node.Addr.String()
	p.logger.Infof("peer %s updated", addr)
}

func ExtractNodeMeta(node *memberlist.Node) (*MemberMeta, error) {
	nodeMeta := &MemberMeta{}
	err := json.Unmarshal(node.Meta, nodeMeta)
	if err != nil {
		return nil, err
	}

	if nodeMeta.IP == "" {
		nodeMeta.IP = node.Addr.String()
	}
	return nodeMeta, nil
}

func (p *peerExchangeMemberManager) syncNode(node *memberlist.Node) {
	member, err := ExtractNodeMeta(node)
	if err != nil {
		p.logger.Errorf("failed to extract node meta %s: %s", string(node.Meta), err)
		return
	}

	if p.memberPool.IsRegistered(member.IP) {
		p.logger.Debugf("node %s is already registered", member.IP)
		return
	}

	grpcClient, peerExchangeClient, err := p.dialMember(member)
	if err != nil {
		p.logger.Errorf("failed to dial %s: %s", node.Addr.String(), err)
		return
	}

	closeFunc := func() error {
		_ = peerExchangeClient.CloseSend()
		return grpcClient.Close()
	}

	err = p.memberPool.Register(member.IP, NewPeerMetadataSendReceiveCloser(peerExchangeClient, closeFunc))
	if errors.Is(err, ErrIsAlreadyExists) {
		p.logger.Debugf("node %s is already registered", member.IP)
		return
	}

	var peerMetadata *dfdaemonv1.PeerMetadata
	for {
		peerMetadata, err = peerExchangeClient.Recv()
		if err != nil {
			return
		}
		p.peerPool.Sync(member, peerMetadata)
	}
}

func (p *peerExchangeMemberManager) dialMember(meta *MemberMeta) (dfdaemonclient.V1, dfdaemonv1.Daemon_PeerExchangeClient, error) {
	formatIP, ok := ip.FormatIP(meta.IP)
	if !ok {
		return nil, nil, fmt.Errorf("failed to format ip: %s", meta.IP)
	}

	netAddr := &dfnet.NetAddr{
		Type: dfnet.TCP,
		Addr: fmt.Sprintf("%s:%d", formatIP, meta.RpcPort),
	}

	credentialOpt := grpc.WithTransportCredentials(p.GRPCCredentials)

	dialCtx, cancel := context.WithTimeout(context.Background(), p.GRPCDialTimeout)
	grpcClient, err := dfdaemonclient.GetV1(dialCtx, netAddr.String(), credentialOpt, grpc.WithBlock())
	cancel()

	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial grpc %s: %s", netAddr.String(), err)
	}

	peerExchangeClient, err := grpcClient.PeerExchange(context.Background())
	if err != nil {
		_ = grpcClient.Close()
		return nil, nil, fmt.Errorf("failed to call %s PeerExchange: %s", netAddr.String(), err)
	}

	return grpcClient, peerExchangeClient, nil
}
