package cloudburstclient

import (
	"errors"
	"fmt"
	"math/rand"
	"time"

	"github.com/golang/protobuf/proto"
	zmq "github.com/pebbe/zmq4"
	. "github.com/proto/common"
)

const (
	pushTemplate = "tcp://%s:%d"
	pullTemplate = "tcp://*:%d"

	requestIdTemplate = "%s:%d"

	routingPort         = 6450
	keyResponsePort     = 6800
	addressResponsePort = 6850

	timeout = 5
)

type PendingRequest struct {
	insertTime time.Time
	address    string
	request    *KeyRequest
}

type AnnaClient struct {
	routingAddress         string
	ipAddress              string
	keyResponseAddress     string
	keyResponseSocket      *zmq.Socket
	addressResponseAddress string
	addressResponseSocket  *zmq.Socket
	poller                 *zmq.Poller
	addressCache           map[string]*[]string
	socketCache            SocketCache
	pendingAddressRequests map[string]*[]PendingRequest
	pendingGetRequests     map[string]*PendingRequest
	pendingPutRequests     map[string]*map[string]PendingRequest
	requestCount           int
	localMode              bool
}

func NewAnnaClient(routingAddress string, ipAddress string, localMode bool) *AnnaClient {
	keyResponseAddress := fmt.Sprintf(pushTemplate, ipAddress, keyResponsePort)
	addressResponseAddress := fmt.Sprintf(pushTemplate, ipAddress, addressResponsePort)

	context, _ := zmq.NewContext()
	keyResponseSocket, _ := context.NewSocket(zmq.PULL)
	keyResponseSocket.Bind(fmt.Sprintf(pullTemplate, keyResponsePort))
	addressResponseSocket, _ := context.NewSocket(zmq.PULL)
	addressResponseSocket.Bind(fmt.Sprintf(pullTemplate, addressResponsePort))

	poller := zmq.NewPoller()
	poller.Add(keyResponseSocket, zmq.POLLIN)
	poller.Add(addressResponseSocket, zmq.POLLIN)

	socketCache := NewSocketCache(context, zmq.PUSH)

	return &AnnaClient{
		routingAddress:         routingAddress,
		ipAddress:              ipAddress,
		keyResponseAddress:     keyResponseAddress,
		keyResponseSocket:      keyResponseSocket,
		addressResponseAddress: addressResponseAddress,
		addressResponseSocket:  addressResponseSocket,
		poller:                 poller,
		addressCache:           map[string]*[]string{},
		socketCache:            socketCache,
		pendingAddressRequests: map[string]*[]PendingRequest{},
		pendingGetRequests:     map[string]*PendingRequest{},
		pendingPutRequests:     map[string]*map[string]PendingRequest{},
		requestCount:           0,
		localMode:              localMode,
	}
}

func (anna *AnnaClient) Put(key string, lattice Lattice, tp LatticeType) bool {
	anna.Put(key, lattice, tp)
	responses := *anna.AsyncReceive()
	for len(responses) == 0 {
		responses = *anna.AsyncReceive()
	}
	response := responses[0]

	return response.Tuples[0].Error == AnnaError_NO_ERROR
}

func (anna *AnnaClient) Get(key string) Lattice {
	anna.AsyncGet(key)
	responses := *anna.AsyncReceive()
	for len(responses) == 0 {
		responses = *anna.AsyncReceive()
	}
	tuple := responses[0].Tuples[0]

	if tuple.Error == AnnaError_KEY_DNE {
		return nil
	}

	if tuple.LatticeType != LatticeType_LWW {
		return nil // We currently only support last-writer wins lattices.
	}

	lww := &LWWValue{}
	proto.Unmarshal(tuple.Payload, lww)
	return &LWWLattice{Timestamp: lww.Timestamp, Value: lww.Value}
}

func (anna *AnnaClient) AsyncGet(key string) {
	_, ok := anna.pendingGetRequests[key]
	if !ok {
		request, _ := anna.prepareDataRequest(key)
		request.Type = RequestType_GET

		anna.tryRequest(request)
	}
}

func (anna *AnnaClient) AsyncPut(key string, lattice Lattice, tp LatticeType) string {
	request, tuple := anna.prepareDataRequest(key)
	request.Type = RequestType_PUT
	tuple.LatticeType = tp
	tuple.Payload = lattice.Serialize()

	anna.tryRequest(request)
	return request.RequestId
}

func (anna *AnnaClient) AsyncReceive() *[]*KeyResponse {
	result := []*KeyResponse{}
	sockets, _ := anna.poller.Poll(100 * time.Millisecond)

	for _, socket := range sockets {
		switch s := socket.Socket; s {
		case anna.keyResponseSocket:
			{
				serialized, _ := anna.keyResponseSocket.RecvBytes(zmq.DONTWAIT)
				response := &KeyResponse{}
				fmt.Println(response)
				proto.Unmarshal(serialized, response)
				key := response.Tuples[0].Key

				if response.Type == RequestType_GET {
					pendingGet, ok := anna.pendingGetRequests[key]
					if ok {
						if anna.validateTuple(response.Tuples[0]) {
							result = append(result, response)
							delete(anna.pendingGetRequests, key)
						} else {
							pendingGet.insertTime = time.Now()
							anna.tryRequest(pendingGet.request)
						}
					}
				} else { // response.Type == PUT
					pendingPuts, ok := anna.pendingPutRequests[key]
					if ok {
						putRequest, ok := (*pendingPuts)[response.ResponseId]
						if ok {
							if anna.validateTuple(response.Tuples[0]) {
								result = append(result, response)
								delete(anna.pendingPutRequests, key)
							} else {
								putRequest.insertTime = time.Now()

								anna.tryRequest(putRequest.request)
							}
						}
					}
				}
			}
		case anna.addressResponseSocket:
			{
				serialized, _ := anna.addressResponseSocket.RecvBytes(zmq.DONTWAIT)
				response := &KeyAddressResponse{}
				proto.Unmarshal(serialized, response)

				key := response.Addresses[0].Key
				pendingRequests, ok := anna.pendingAddressRequests[key]
				if ok {
					if response.Error == AnnaError_NO_SERVERS {
						anna.asyncQueryRouting(key)
					} else {
						// Populate the local address cache.
						anna.addressCache[key] = &response.Addresses[0].Ips

						// Retry all the requests we made pending before getting this
						// response.
						for _, request := range *pendingRequests {
							fmt.Println(request.request)
							anna.tryRequest(request.request)
						}

						delete(anna.pendingAddressRequests, key)
					}
				}
			}
		}
	}

	// TODO: Check for timeouts.

	return &result
}

func (anna *AnnaClient) prepareDataRequest(key string) (*KeyRequest, *KeyTuple) {
	tuple := &KeyTuple{
		Key: key,
	}

	request := &KeyRequest{
		RequestId:       anna.getRequestId(),
		ResponseAddress: anna.keyResponseAddress,
		Tuples:          []*KeyTuple{tuple},
	}

	return request, tuple

}

func (anna *AnnaClient) tryRequest(request *KeyRequest) {
	key := request.Tuples[0].Key
	address, err := anna.getWorkerAddress(key)
	if err != nil { // We didn't have an address cached locally.
		pending := PendingRequest{insertTime: time.Now(), address: "", request: request}
		requests, ok := anna.pendingAddressRequests[key]
		if !ok {
			requests = &[]PendingRequest{}
			anna.pendingAddressRequests[key] = requests
		}
		*requests = append(*requests, pending)

		return
	}

	sckt := anna.socketCache.Get(address)
	serialized, _ := proto.Marshal(request)
	proto.Unmarshal(serialized, request)
	fmt.Println("post unmarshaling\n", request)
	sckt.SendBytes(serialized, zmq.DONTWAIT)

	pendingRequest := PendingRequest{insertTime: time.Now(), address: address, request: request}
	if request.Type == RequestType_GET {
		anna.pendingGetRequests[key] = &pendingRequest
	} else {
		pendingMap, ok := anna.pendingPutRequests[key]
		if !ok {
			pendingMap = &map[string]PendingRequest{}
			anna.pendingPutRequests[key] = pendingMap
		}
		(*pendingMap)[request.RequestId] = pendingRequest
	}
}

func (anna *AnnaClient) getWorkerAddress(key string) (string, error) {
	addresses, ok := anna.addressCache[key]
	if !ok {
		anna.asyncQueryRouting(key)

		return "", errors.New("No address found.")
	}

	return (*addresses)[rand.Intn(len(*addresses))], nil
}

func (anna *AnnaClient) asyncQueryRouting(key string) {
	port := routingPort
	if !anna.localMode {
		port = port + rand.Intn(4)
	}

	address := fmt.Sprintf(pushTemplate, anna.routingAddress, port)
	socket := anna.socketCache.Get(address)

	request := &KeyAddressRequest{
		RequestId:       anna.getRequestId(),
		ResponseAddress: anna.addressResponseAddress,
		Keys:            []string{key},
	}

	serialized, _ := proto.Marshal(request)
	socket.SendBytes(serialized, zmq.DONTWAIT)
}

func (anna *AnnaClient) getRequestId() string {
	id := fmt.Sprintf(requestIdTemplate, anna.ipAddress, anna.requestCount)
	anna.requestCount++

	if anna.requestCount == 10000 {
		anna.requestCount = 0
	}

	return id
}

func (anna *AnnaClient) validateTuple(tuple *KeyTuple) bool {
	key := tuple.Key

	if tuple.Error == AnnaError_WRONG_THREAD {
		delete(anna.addressCache, key)
		return false
	}

	if tuple.Invalidate {
		delete(anna.addressCache, key)
	}

	return true
}
