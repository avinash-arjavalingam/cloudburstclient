package cloudburstclient

import (
	"errors"
	"fmt"

	"github.com/golang/protobuf/proto"
	. "github.com/proto/common"
)

type Lattice interface {
	Reveal() interface{}

	Assign(val interface{}) error

	Merge(other Lattice) (Lattice, error)

	Serialize() []byte
}

type LWWLattice struct {
	Timestamp uint64
	Value     []byte
}

func (lattice *LWWLattice) Reveal() interface{} {
	return lattice.Value
}

func (lattice *LWWLattice) Assign(val interface{}) error {
	bts, ok := val.([]byte)
	if !ok {
		return errors.New(fmt.Sprintf("Unexpected type %T in LWWLattice's assign function.", val))
	}

	lattice.Value = bts
	return nil
}

func (lattice *LWWLattice) Merge(other Lattice) (Lattice, error) {
	lww, ok := other.(*LWWLattice)
	if !ok {
		return nil, errors.New(fmt.Sprintf("Unexpected type %T in LWWLattice's merge function. Only accepts other LWWLatttices.", lww))
	}

	if lww.Timestamp > lattice.Timestamp {
		lattice.Value = lww.Value
	}

	return lattice, nil
}

func (lattice *LWWLattice) Serialize() []byte {
	pb := &LWWValue{Timestamp: lattice.Timestamp, Value: lattice.Value}

	serialized, _ := proto.Marshal(pb)
	return serialized
}
