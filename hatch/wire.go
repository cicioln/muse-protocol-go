package hatch

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// Protobuf shapes (from the web client's descriptors):
//
//	NoiseTransportFrame { int64 chunk_id=1; uint32 chunk_index=2; uint32 total_chunks=3; bytes payload=4; }
//	ServiceRequest      { ServiceType service=1; bytes payload=2; }
//	ServiceResponse     { bytes payload=1; }
//	ServiceFrame        { int64 stream_id=1; oneof { ApplicationRequest request=2;
//	                      ApplicationResponse response=3; BodyChunk body_chunk=4; Reset reset=5; } }
//	ApplicationRequest  { string verb=1; string path=2; repeated Header headers=3; bytes body=4; bool end_body=5; }
//	ApplicationResponse { int32 status=1; repeated Header headers=2; bytes body=3; bool end_body=4; }
//	BodyChunk           { bytes data=1; bool end_body=2; }
//	Header              { string key=1; string value=2; }
//	Reset               { Code code=1; string reason=2; }

type Service int

const (
	ServiceDaemon   Service = 0
	ServiceSentinel Service = 1
	ServiceVault    Service = 2
	ServiceAuthd    Service = 3
)

const (
	maxChunkPayload  = 4 << 20
	sendChunkPayload = 48 << 10
	maxChunks        = 4096
	maxAssembly      = 256 << 20
)

// ---- minimal protobuf encoding

type pbuf []byte

func (b pbuf) varint(v uint64) pbuf { return binary.AppendUvarint(b, v) }
func (b pbuf) tag(field, wire int) pbuf {
	return b.varint(uint64(field<<3 | wire))
}
func (b pbuf) uint(field int, v uint64) pbuf { return b.tag(field, 0).varint(v) }
func (b pbuf) bytes(field int, v []byte) pbuf {
	return append(b.tag(field, 2).varint(uint64(len(v))), v...)
}
func (b pbuf) str(field int, v string) pbuf { return b.bytes(field, []byte(v)) }
func (b pbuf) boolean(field int, v bool) pbuf {
	if v {
		return b.uint(field, 1)
	}
	return b.uint(field, 0)
}

type pbField struct {
	num  int
	wire int
	v    uint64
	b    []byte
}

func parsePB(data []byte) ([]pbField, error) {
	var out []pbField
	for len(data) > 0 {
		tag, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, errors.New("protobuf: bad tag")
		}
		data = data[n:]
		f := pbField{num: int(tag >> 3), wire: int(tag & 7)}
		switch f.wire {
		case 0:
			v, n := binary.Uvarint(data)
			if n <= 0 {
				return nil, errors.New("protobuf: bad varint")
			}
			f.v, data = v, data[n:]
		case 1:
			if len(data) < 8 {
				return nil, errors.New("protobuf: truncated fixed64")
			}
			f.b, data = data[:8], data[8:]
		case 2:
			l, n := binary.Uvarint(data)
			if n <= 0 || uint64(len(data)-n) < l {
				return nil, errors.New("protobuf: truncated bytes")
			}
			f.b, data = data[n:n+int(l)], data[n+int(l):]
		case 5:
			if len(data) < 4 {
				return nil, errors.New("protobuf: truncated fixed32")
			}
			f.b, data = data[:4], data[4:]
		default:
			return nil, fmt.Errorf("protobuf: unsupported wire type %d", f.wire)
		}
		out = append(out, f)
	}
	return out, nil
}

// ---- application layer

type Header struct{ Key, Value string }

type AppRequest struct {
	Verb    string
	Path    string
	Headers []Header
	Body    []byte
	EndBody bool
}

func (r *AppRequest) encode() []byte {
	b := pbuf{}.str(1, r.Verb).str(2, r.Path)
	for _, h := range r.Headers {
		b = b.bytes(3, pbuf{}.str(1, h.Key).str(2, h.Value))
	}
	if len(r.Body) > 0 {
		b = b.bytes(4, r.Body)
	}
	return b.boolean(5, r.EndBody)
}

type AppResponse struct {
	Status  int
	Headers []Header
	Body    []byte
	EndBody bool
}

func decodeHeader(b []byte) Header {
	var h Header
	fs, _ := parsePB(b)
	for _, f := range fs {
		switch f.num {
		case 1:
			h.Key = string(f.b)
		case 2:
			h.Value = string(f.b)
		}
	}
	return h
}

func decodeAppRequest(b []byte) (*AppRequest, error) {
	fs, err := parsePB(b)
	if err != nil {
		return nil, err
	}
	r := &AppRequest{}
	for _, f := range fs {
		switch f.num {
		case 1:
			r.Verb = string(f.b)
		case 2:
			r.Path = string(f.b)
		case 3:
			r.Headers = append(r.Headers, decodeHeader(f.b))
		case 4:
			r.Body = append([]byte(nil), f.b...)
		case 5:
			r.EndBody = f.v != 0
		}
	}
	return r, nil
}

func (r *AppResponse) encode() []byte {
	b := pbuf{}.uint(1, uint64(r.Status))
	for _, h := range r.Headers {
		b = b.bytes(2, pbuf{}.str(1, h.Key).str(2, h.Value))
	}
	if len(r.Body) > 0 {
		b = b.bytes(3, r.Body)
	}
	return b.boolean(4, r.EndBody)
}

func decodeAppResponse(b []byte) (*AppResponse, error) {
	fs, err := parsePB(b)
	if err != nil {
		return nil, err
	}
	r := &AppResponse{}
	for _, f := range fs {
		switch f.num {
		case 1:
			r.Status = int(int32(f.v))
		case 2:
			r.Headers = append(r.Headers, decodeHeader(f.b))
		case 3:
			r.Body = append([]byte(nil), f.b...)
		case 4:
			r.EndBody = f.v != 0
		}
	}
	return r, nil
}

// Frame is a decoded ServiceFrame.
type Frame struct {
	StreamID int64
	Request  *AppRequest
	Response *AppResponse
	Chunk    []byte
	ChunkEnd bool
	IsChunk  bool
	Reset    *Reset
}

type Reset struct {
	Code   int
	Reason string
}

func (f *Frame) encode() []byte {
	b := pbuf{}.uint(1, uint64(f.StreamID))
	switch {
	case f.Request != nil:
		b = b.bytes(2, f.Request.encode())
	case f.Response != nil:
		b = b.bytes(3, f.Response.encode())
	case f.IsChunk:
		b = b.bytes(4, pbuf{}.bytes(1, f.Chunk).boolean(2, f.ChunkEnd))
	case f.Reset != nil:
		b = b.bytes(5, pbuf{}.uint(1, uint64(f.Reset.Code)).str(2, f.Reset.Reason))
	}
	return b
}

func decodeFrame(b []byte) (*Frame, error) {
	fs, err := parsePB(b)
	if err != nil {
		return nil, err
	}
	fr := &Frame{}
	for _, f := range fs {
		switch f.num {
		case 1:
			fr.StreamID = int64(f.v)
		case 2:
			if fr.Request, err = decodeAppRequest(f.b); err != nil {
				return nil, err
			}
		case 3:
			if fr.Response, err = decodeAppResponse(f.b); err != nil {
				return nil, err
			}
		case 4:
			fr.IsChunk = true
			inner, err := parsePB(f.b)
			if err != nil {
				return nil, err
			}
			for _, g := range inner {
				switch g.num {
				case 1:
					fr.Chunk = append([]byte(nil), g.b...)
				case 2:
					fr.ChunkEnd = g.v != 0
				}
			}
		case 5:
			fr.Reset = &Reset{}
			inner, _ := parsePB(f.b)
			for _, g := range inner {
				switch g.num {
				case 1:
					fr.Reset.Code = int(g.v)
				case 2:
					fr.Reset.Reason = string(g.b)
				}
			}
		}
	}
	return fr, nil
}

// EncodeRequest wraps a frame for the client→gateway direction.
func EncodeRequest(svc Service, f *Frame) []byte {
	return pbuf{}.uint(1, uint64(svc)).bytes(2, f.encode())
}

// DecodeRequest is the gateway side of EncodeRequest.
func DecodeRequest(b []byte) (Service, *Frame, error) {
	fs, err := parsePB(b)
	if err != nil {
		return 0, nil, err
	}
	var svc Service
	var payload []byte
	for _, f := range fs {
		switch f.num {
		case 1:
			svc = Service(f.v)
		case 2:
			payload = f.b
		}
	}
	fr, err := decodeFrame(payload)
	return svc, fr, err
}

// EncodeResponse wraps a frame for the gateway→client direction.
func EncodeResponse(f *Frame) []byte { return pbuf{}.bytes(1, f.encode()) }

// DecodeResponse is the client side of EncodeResponse.
func DecodeResponse(b []byte) (*Frame, error) {
	fs, err := parsePB(b)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if f.num == 1 && f.wire == 2 {
			return decodeFrame(f.b)
		}
	}
	return nil, errors.New("ServiceResponse without payload")
}

// ---- Noise transport chunking

func randomInt64() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

// SplitChunks frames a message as NoiseTransportFrames of at most size bytes.
func SplitChunks(data []byte, size int) [][]byte {
	total := (len(data) + size - 1) / size
	if total == 0 {
		total = 1
	}
	id := uint64(randomInt64())
	out := make([][]byte, 0, total)
	for i := 0; i < total; i++ {
		end := min((i+1)*size, len(data))
		part := data[min(i*size, len(data)):end]
		out = append(out, pbuf{}.uint(1, id).uint(2, uint64(i)).uint(3, uint64(total)).bytes(4, part))
	}
	return out
}

type assembly struct {
	total int
	parts map[int][]byte
	size  int
}

// Reassembler joins chunked NoiseTransportFrames back into messages.
type Reassembler struct {
	pending  map[int64]*assembly
	order    []int64
	buffered int
}

// Feed returns the full message once its last chunk arrives, else nil.
func (r *Reassembler) Feed(raw []byte) ([]byte, error) {
	fs, err := parsePB(raw)
	if err != nil {
		return nil, err
	}
	var id int64
	idx, total := 0, 1
	var payload []byte
	for _, f := range fs {
		switch f.num {
		case 1:
			id = int64(f.v)
		case 2:
			idx = int(f.v)
		case 3:
			total = int(f.v)
		case 4:
			payload = f.b
		}
	}
	if total < 1 || total > maxChunks || idx < 0 || idx >= total || len(payload) > maxChunkPayload {
		return nil, errors.New("invalid noise chunk")
	}
	if total == 1 {
		if payload == nil {
			payload = []byte{} // nil means "incomplete" to callers
		}
		return payload, nil
	}
	if r.pending == nil {
		r.pending = map[int64]*assembly{}
	}
	a := r.pending[id]
	if a == nil {
		if len(r.pending) >= 32 { // bound memory held by abandoned messages
			r.remove(r.order[0])
		}
		a = &assembly{total: total, parts: map[int][]byte{}}
		r.pending[id] = a
		r.order = append(r.order, id)
	}
	if a.total != total || a.parts[idx] != nil {
		r.remove(id)
		return nil, errors.New("inconsistent noise chunks")
	}
	if len(payload) > maxAssembly-a.size || len(payload) > maxAssembly-r.buffered {
		r.remove(id)
		return nil, errors.New("noise message buffer too large")
	}
	a.parts[idx] = append([]byte(nil), payload...)
	a.size += len(payload)
	r.buffered += len(payload)
	if len(a.parts) < total {
		return nil, nil
	}
	r.remove(id)
	out := make([]byte, 0, a.size)
	for i := 0; i < total; i++ {
		out = append(out, a.parts[i]...)
	}
	return out, nil
}

func (r *Reassembler) remove(id int64) {
	if a := r.pending[id]; a != nil {
		r.buffered -= a.size
	}
	delete(r.pending, id)
	for i, v := range r.order {
		if v == id {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}
