package wirekit_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"wirekit"
)

func TestVarintRoundTrip(t *testing.T) {
	for _, v := range []uint64{0, 1, 127, 128, 300, 16383, 16384, 1 << 32} {
		buf := wirekit.AppendVarint(nil, v)
		got, n, err := wirekit.ReadVarint(buf)
		if err != nil {
			t.Fatalf("v=%d: ReadVarint = %v", v, err)
		}
		if got != v {
			t.Fatalf("v=%d: 读回 %d", v, got)
		}
		if n != len(buf) {
			t.Fatalf("v=%d: 消耗 %d 字节，编码长度是 %d", v, n, len(buf))
		}
	}
}

func TestAppendFrameRoundTrip(t *testing.T) {
	want := wirekit.Frame{Type: 3, Payload: []byte("hello")}

	buf, err := wirekit.AppendFrame(nil, want)
	if err != nil {
		t.Fatalf("AppendFrame: %v", err)
	}

	got, n, err := wirekit.ReadFrameFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrameFrom: %v", err)
	}
	if n != len(buf) {
		t.Fatalf("消耗 %d 字节，整段是 %d 字节", n, len(buf))
	}
	if got.Type != want.Type {
		t.Fatalf("Type = %d, want %d", got.Type, want.Type)
	}
	if !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("Payload = %q, want %q", got.Payload, want.Payload)
	}
}

func TestReadFrameFromStopsAtFrameEnd(t *testing.T) {
	buf, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 1, Payload: []byte("abc")})
	if err != nil {
		t.Fatalf("AppendFrame: %v", err)
	}
	tail := []byte("TAIL")
	buf = append(buf, tail...)

	_, n, err := wirekit.ReadFrameFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrameFrom: %v", err)
	}
	if !bytes.Equal(buf[n:], tail) {
		t.Fatalf("消耗 %d 字节之后剩下 %q，期望 %q", n, buf[n:], tail)
	}
}

func TestReadFrameFromShortBuffer(t *testing.T) {
	buf, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 1, Payload: []byte("abc")})
	if err != nil {
		t.Fatalf("AppendFrame: %v", err)
	}

	if _, n, err := wirekit.ReadFrameFrom(buf[:len(buf)-1]); !errors.Is(err, wirekit.ErrShortBuffer) {
		t.Fatalf("载荷缺一个字节时返回 (%d, %v)，期望 ErrShortBuffer", n, err)
	}
	if _, n, err := wirekit.ReadFrameFrom(nil); !errors.Is(err, wirekit.ErrShortBuffer) {
		t.Fatalf("空缓冲区返回 (%d, %v)，期望 ErrShortBuffer", n, err)
	}
}

func TestAppendFrameRejectsBadType(t *testing.T) {
	for _, typ := range []uint64{0, wirekit.MaxType + 1} {
		if _, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: typ}); !errors.Is(err, wirekit.ErrBadType) {
			t.Fatalf("Type=%d 返回 %v，期望 ErrBadType", typ, err)
		}
	}
}

func TestReadFrameFromReader(t *testing.T) {
	var stream []byte
	for i := 1; i <= 3; i++ {
		var err error
		stream, err = wirekit.AppendFrame(stream, wirekit.Frame{
			Type:    uint64(i),
			Payload: bytes.Repeat([]byte{byte('a' + i)}, i*3),
		})
		if err != nil {
			t.Fatalf("AppendFrame: %v", err)
		}
	}

	r := bytes.NewReader(stream)
	for i := 1; i <= 3; i++ {
		f, err := wirekit.ReadFrame(r)
		if err != nil {
			t.Fatalf("第 %d 帧: %v", i, err)
		}
		if int(f.Type) != i {
			t.Fatalf("第 %d 帧的 Type = %d", i, f.Type)
		}
		if !bytes.Equal(f.Payload, bytes.Repeat([]byte{byte('a' + i)}, i*3)) {
			t.Fatalf("第 %d 帧的 Payload = %q", i, f.Payload)
		}
	}
	if _, err := wirekit.ReadFrame(r); err != io.EOF {
		t.Fatalf("流读完之后返回 %v，期望 io.EOF", err)
	}
}
