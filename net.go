package grpcx

import "net"

var _ net.Addr = &Addr{}

type Addr struct {
	network string
	address string
}

func NewAddr(network, address string) net.Addr {
	return &Addr{network, address}
}

func (x *Addr) Network() string {
	return x.network
}

func (x *Addr) String() string {
	return x.address
}
