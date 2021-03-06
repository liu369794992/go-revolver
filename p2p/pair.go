/**
 * File        : pair.go
 * Description : Service for pairing artifact streams.
 * Copyright   : Copyright (c) 2017-2018 DFINITY Stiftung. All rights reserved.
 * Maintainer  : Enzo Haussecker <enzo@dfinity.org>
 * Stability   : Experimental
 */

package p2p

import (
	"fmt"

	"gx/ipfs/QmNa31VPzC561NWwRsJLE7nGYZYuuD2QfpK2b1q9BK54J1/go-libp2p-net"
	"gx/ipfs/QmXYjuNuxVzXKJCfWasQk1RqkhVLDM9jtUKhqc2WPQmFSB/go-libp2p-peer"

	"github.com/dfinity/go-revolver/util"
)

const (
	ack = 0x06
	nak = 0x15
)

// Request to exchange artifacts with a peer.
func (client *client) pair(peerId peer.ID) (bool, error) {

	// Log this action.
	pid := peerId
	client.logger.Debug("Requesting to pair with", pid)

	// Connect to the target peer.
	stream, err := client.host.NewStream(
		client.context,
		pid,
		client.protocol+"/pair",
	)
	if err != nil {
		addrs := client.peerstore.PeerInfo(pid).Addrs
		client.logger.Debug("Cannot connect to", pid, "at", addrs, err)
		client.peerstore.ClearAddrs(pid)
		client.table.Remove(pid)
		return false, err
	}

	// Receive data from the target peer.
	data, err := util.ReadWithTimeout(
		stream,
		1,
		client.config.Timeout,
	)
	if err != nil {
		client.logger.Warning("Cannot receive data from", pid, err)
		stream.Close()
		return false, err
	}

	// Add the outbound stream to the stream store.
	var success bool
	if data[0] == ack && client.streamstore.Add(pid, stream, true) {

		// Ready to send artifacts.
		client.logger.Debug("Ready to exchange artifacts with", pid)
		go client.process(stream)
		success = true

	} else {

		// Cannot pair with the target peer.
		client.logger.Debug("Cannot pair with", pid)
		stream.Close()
		success = false

	}

	// Return without error.
	return success, nil

}

// Handle incomming pairing requests.
func (client *client) pairHandler(stream net.Stream) {

	// Log this action.
	pid := stream.Conn().RemotePeer()
	client.logger.Debug("Receiving request to pair with", pid)

	// Prepare to reject the request.
	reject := func(reason ...interface{}) {
		client.logger.Debug("Cannot pair with", pid, "because", fmt.Sprint(reason...))
		err := util.WriteWithTimeout(
			stream,
			[]byte{nak},
			client.config.Timeout,
		)
		if err != nil {
			client.logger.Warning("Cannot send data to", pid, err)
		}
		stream.Close()
	}

	// Add the inbound stream to the stream store.
	if !client.streamstore.Add(pid, stream, false) {
		reject(pid, " cannot be added to the stream store")
		return
	}

	// Send an acknowledgement.
	err := util.WriteWithTimeout(
		stream,
		[]byte{ack},
		client.config.Timeout,
	)
	if err != nil {
		client.logger.Warning("Cannot send data to", pid, err)
		client.streamstore.Remove(pid)
		return
	}

	// Ready to exchange artifacts.
	client.logger.Debug("Ready to exchange artifacts with", pid)
	go client.process(stream)
	return

}

// Register the pairing handler.
func (client *client) registerPairService() {
	uri := client.protocol + "/pair"
	client.host.SetStreamHandler(uri, client.pairHandler)
}
