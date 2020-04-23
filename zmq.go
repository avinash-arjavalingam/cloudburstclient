package cloudburstclient

import (
	zmq "github.com/pebbe/zmq4"
)

type SocketCache struct {
	sockets map[string]*zmq.Socket
	context *zmq.Context
	zmqType zmq.Type
}

func NewSocketCache(context *zmq.Context, tp zmq.Type) SocketCache {
	return SocketCache{sockets: map[string]*zmq.Socket{}, context: context, zmqType: tp}
}

func (socketCache *SocketCache) Get(address string) *zmq.Socket {
	socket, ok := socketCache.sockets[address]
	if !ok {
		socket, _ = socketCache.context.NewSocket(socketCache.zmqType)
		socket.Connect(address)

		socketCache.sockets[address] = socket
	}

	return socket
}
