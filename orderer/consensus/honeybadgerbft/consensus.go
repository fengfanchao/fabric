/*
Copyright IBM Corp. 2016 All Rights Reserved.
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
                 http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package honeybadgerbft

import (
	"fmt"
	"sync"
	"time"

	cb "github.com/hyperledger/fabric/protos/common"
	"github.com/op/go-logging"

	"encoding/binary"
	"io"
	"net"

	localconfig "github.com/hyperledger/fabric/orderer/common/localconfig"
	"github.com/hyperledger/fabric/orderer/consensus"
	"github.com/hyperledger/fabric/protos/utils"
)

var logger = logging.MustGetLogger("orderer/honeybadgerbft")
var sendSocketPath = ""
var receiveSocketPath = ""

//measurements
var interval = int64(10000)
var envelopeMeasurementStartTime = int64(-1)
var countEnvelopes = int64(0)

type consenter struct{}

type chain struct {
	support           consensus.ConsenterSupport
	sendChan          chan *cb.Block
	exitChan          chan struct{}
	sendConnection    net.Conn
	receiveConnection net.Conn
	sendLock          *sync.Mutex
}

// New creates a new consenter for the HoneyBadgerBFT consensus scheme.
// It communicates with a HoneyBadgerBFT node via Unix websockets and simply marshals/sends and receives/unmarshals
// messages.
func New(config localconfig.HoneyBadgerBFT) consensus.Consenter {
	sendSocketPath = config.SendSocketPath
	receiveSocketPath = config.ReceiveSocketPath
	return &consenter{}
}

func (consenter *consenter) HandleChain(support consensus.ConsenterSupport, metadata *cb.Metadata) (consensus.Chain, error) {
	return newChain(support), nil
}

func newChain(support consensus.ConsenterSupport) *chain {
	return &chain{
		support:  support,
		sendChan: make(chan *cb.Block),
		exitChan: make(chan struct{}),
		sendLock: &sync.Mutex{},
	}
}

func (ch *chain) Start() {
	conn, err := net.Dial("unix", sendSocketPath)

	if err != nil {
		logger.Debugf("Could not connect to send proxy!")
		return
	} else {
		logger.Debugf("Connected to send proxy!")
	}

	ch.sendConnection = conn

	conn, err = net.Dial("unix", receiveSocketPath)

	if err != nil {
		logger.Debugf("Could not connect to receive proxy!")
		return
	} else {
		logger.Debugf("Connected to receive proxy!")
	}

	ch.receiveConnection = conn

	go ch.connLoop()

	go ch.appendToChain()
}

func (ch *chain) Halt() {

	select {
	case <-ch.exitChan:
		// Allow multiple halts without panic
	default:
		close(ch.exitChan)
	}
}

// Configure accepts configuration update messages for ordering
// TODO
func (ch *chain) Configure(config *cb.Envelope, configSeq uint64) error {
	//select {
	//case ch.sendChan <- &message{
	//	configSeq: configSeq,
	//	configMsg: config,
	//}:
	//	return nil
	//case <-ch.exitChan:
	//	return fmt.Errorf("Exiting")
	//}

	return nil
}

// Errored only closes on exit
func (ch *chain) Errored() <-chan struct{} {
	return ch.exitChan
}

func (ch *chain) sendLength(length int, conn net.Conn) (int, error) {
	var buf [8]byte

	binary.BigEndian.PutUint64(buf[:], uint64(length))

	return conn.Write(buf[:])
}

func (ch *chain) sendEnvToBFTProxy(env *cb.Envelope) (int, error) {
	ch.sendLock.Lock()
	bytes, err := utils.Marshal(env)

	if err != nil {
		return -1, err
	}

	status, err := ch.sendLength(len(bytes), ch.sendConnection)

	if err != nil {
		return status, err
	}

	i, err := ch.sendConnection.Write(bytes)

	ch.sendLock.Unlock()

	return i, err
}

func (ch *chain) recvLength() (int64, error) {
	var size int64
	err := binary.Read(ch.receiveConnection, binary.BigEndian, &size)
	return size, err
}

func (ch *chain) recvBytes() ([]byte, error) {
	size, err := ch.recvLength()

	if err != nil {
		return nil, err
	}

	buf := make([]byte, size)

	_, err = io.ReadFull(ch.receiveConnection, buf)

	if err != nil {
		return nil, err
	}

	return buf, nil
}

func (ch *chain) recvEnvFromBFTProxy() (*cb.Envelope, error) {
	size, err := ch.recvLength()

	if err != nil {
		return nil, err
	}

	buf := make([]byte, size)

	_, err = io.ReadFull(ch.receiveConnection, buf)

	if err != nil {
		return nil, err
	}

	env, err := utils.UnmarshalEnvelope(buf)

	if err != nil {
		return nil, err
	}

	return env, nil
}

// Order accepts a message and returns true on acceptance, or false on shutdown
func (ch *chain) Order(env *cb.Envelope, _ uint64) error {

	_, err := ch.sendEnvToBFTProxy(env)

	if err != nil {
		return err
	}

	if envelopeMeasurementStartTime == -1 {
		envelopeMeasurementStartTime = time.Now().UnixNano()
	}

	countEnvelopes++
	if countEnvelopes%interval == 0 {

		tp := float64(interval*1000000000) / float64(time.Now().UnixNano()-envelopeMeasurementStartTime)
		fmt.Printf("Throughput = %v envelopes/sec\n", tp)
		envelopeMeasurementStartTime = time.Now().UnixNano()

	}

	select {
	case <-ch.exitChan:
		return fmt.Errorf("exiting")
	default: //JCS: avoid blocking
		return nil
	}
}

func (ch *chain) connLoop() {
	for {
		// receive a marshalled block
		bytes, err := ch.recvBytes()
		if err != nil {
			logger.Debugf("[recv] Error while receiving block from HoneyBadgerBFT proxy: %v\n", err)
			continue
		}

		block, err := utils.GetBlockFromBlockBytes(bytes)
		if err != nil {
			logger.Debugf("[recv] Error while unmarshaling block from HoneyBadgerBFT proxy: %v\n", err)
			continue
		}

		ch.sendChan <- block
	}
}

func (ch *chain) appendToChain() {
	//var timer <-chan time.Time //JCS: original timer to flush the blockcutter

	for {
		select {
		case block := <-ch.sendChan:

			err := ch.support.AppendBlock(block)
			if err != nil {
				logger.Panicf("Could not append block: %s", err)
			}

		case <-ch.exitChan:
			logger.Debugf("Exiting...")
			return
		}
	}
}
