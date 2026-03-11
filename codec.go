package main

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
	_ "google.golang.org/grpc/encoding/proto"
)

// jsonCodec replaces the default "proto" codec with JSON marshaling.
// Must match the coordinator's codec for interoperability.
type jsonCodec struct{}

func init() {
	encoding.RegisterCodec(jsonCodec{})
}

func (jsonCodec) Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

func (jsonCodec) Unmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

func (jsonCodec) Name() string {
	return "proto"
}
