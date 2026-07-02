package driver

type Sender interface {
	Send(uint32, []byte) error
}
