package driver

type Marshaler interface {
	Marshal(msg any) ([]byte, error)
}

type Unmarshaler interface {
	Unmarshal(p []byte, msg any) error
}
