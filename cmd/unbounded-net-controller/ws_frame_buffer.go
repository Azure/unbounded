// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"

	"github.com/coder/websocket"
	"k8s.io/klog/v2"
)

const (
	maxNodeWSFrameBytes   = 2 * 1024 * 1024
	nodeWSBufferClasses   = 9
	nodeWSMinBufferSize   = 8 * 1024
	nodeWSBuffersPerClass = 16
)

// Idle buffers are capped below 64 MiB across all connections. Active frames
// retain exclusive ownership until processing finishes, not for a connection's lifetime.
type nodeWSBufferPool struct {
	buffers [nodeWSBufferClasses]chan []byte
}

var nodeWSBuffers = newNodeWSBufferPool(nodeWSBuffersPerClass)

func newNodeWSBufferPool(perClass int) *nodeWSBufferPool {
	pool := &nodeWSBufferPool{}
	for i := range pool.buffers {
		pool.buffers[i] = make(chan []byte, perClass)
	}

	return pool
}

func (p *nodeWSBufferPool) get(size int) []byte {
	for i, buffers := range p.buffers {
		classSize := nodeWSMinBufferSize << i
		if classSize < size {
			continue
		}

		select {
		case data := <-buffers:
			return data[:0]
		default:
			return make([]byte, 0, classSize)
		}
	}

	panic("node WebSocket buffer request exceeds the frame limit")
}

func (p *nodeWSBufferPool) put(data []byte) {
	for i, buffers := range p.buffers {
		if cap(data) != nodeWSMinBufferSize<<i {
			continue
		}

		select {
		case buffers <- data[:0]:
		default:
		}

		return
	}
}

func (p *nodeWSBufferPool) read(reader io.Reader) ([]byte, error) {
	data := p.get(nodeWSMinBufferSize)
	emptyReads := 0

	for {
		if len(data) == maxNodeWSFrameBytes {
			// Any extra byte invalidates the frame, so the probe can reuse its storage.
			n, err := reader.Read(data[:1])
			if n != 0 {
				p.put(data)
				return nil, websocket.ErrMessageTooBig
			}

			if err == io.EOF {
				return data, nil
			}

			if err != nil {
				p.put(data)
				return nil, err
			}
		} else {
			if len(data) == cap(data) {
				larger := p.get(cap(data) * 2)
				larger = append(larger, data...)
				p.put(data)
				data = larger
			}

			n, err := reader.Read(data[len(data):cap(data)])

			data = data[:len(data)+n]
			if err == io.EOF {
				return data, nil
			}

			if err != nil {
				p.put(data)
				return nil, err
			}

			if n > 0 {
				emptyReads = 0
				continue
			}
		}

		emptyReads++
		if emptyReads >= 100 {
			p.put(data)
			return nil, io.ErrNoProgress
		}
	}
}

func (p *nodeWSBufferPool) readFrame(ctx context.Context, conn *websocket.Conn) (wsFrame, error) {
	msgType, reader, err := conn.Reader(ctx)
	if err != nil {
		return wsFrame{}, err
	}

	data, err := p.read(reader)
	if errors.Is(err, websocket.ErrMessageTooBig) {
		if closeErr := conn.Close(websocket.StatusMessageTooBig, "node status exceeds 2 MiB"); closeErr != nil {
			klog.V(4).Infof("Node WebSocket oversized message close failed: %v", closeErr)
		}
	}

	return wsFrame{msgType: msgType, data: data}, err
}
