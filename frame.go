// Package wirekit 是一个流式二进制帧编解码器。
//
// 帧的线路格式是：
//
//	+-----------+------------+------------------+
//	| varint    | varint     | 载荷             |
//	| 帧类型     | 载荷长度    | 载荷长度个字节     |
//	+-----------+------------+------------------+
//
// 详细语义见 README.md 的「对外契约」一节（9 条）。
package wirekit

import (
	"io"
)

const (
	// MaxType 是允许的帧类型上限（下限是 1）。
	MaxType = 15

	// MaxPayload 是一帧载荷的字节数上限，用于内存里的帧
	// （AppendFrame / ReadFrameFrom / ReadFrame）。
	MaxPayload = 1 << 20

	// MaxStreamPayload 是 CopyFrame 允许的载荷字节数上限。
	// 它比 MaxPayload 大得多：CopyFrame 是流式转发，内存占用与声明长度无关。
	MaxStreamPayload = 1 << 30

	// maxVarintLen 是 uint64 的 uvarint 编码的最大字节数。
	maxVarintLen = 10

	// copyBufSize 是 CopyFrame 流式转发用的临时缓冲上限（32 KiB）。
	copyBufSize = 32 << 10
)

// Frame 是一帧。
type Frame struct {
	// Type 是帧类型，取值 [1, MaxType]。
	Type uint64

	// Payload 是载荷。
	//
	// 由 ReadFrameFrom 返回时，Payload 与传入的 buf 共享底层数组（零拷贝）；
	// 由 ReadFrame 返回时，Payload 是新分配的。
	Payload []byte
}

// AppendVarint 把 v 的 uvarint 编码追加到 dst 末尾，并返回扩展后的切片。
//
// 编码方式与 encoding/binary 的 uvarint 一致：每字节 7 位，低位在前，
// 最高位表示后面还有没有字节。
func AppendVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// ReadVarint 从 buf 的开头读出一个 uvarint，返回它消耗的字节数。
//
// 出错时的返回值为 (0, 0, err)。
func ReadVarint(buf []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(buf); i++ {
		if i == maxVarintLen {
			return 0, 0, ErrVarintOverflow
		}
		b := buf[i]
		if b < 0x80 {
			if i == maxVarintLen-1 && b > 1 {
				return 0, 0, ErrVarintOverflow
			}
			return v | uint64(b)<<(7*i), i + 1, nil
		}
		v |= uint64(b&0x7f) << (7 * i)
	}
	return 0, 0, ErrShortBuffer
}

// AppendFrame 把一帧编码后追加到 dst 末尾。
//
// 出错时 dst 保持不变。
func AppendFrame(dst []byte, f Frame) ([]byte, error) {
	if f.Type < 1 || f.Type > MaxType {
		return dst, ErrBadType
	}
	if len(f.Payload) > MaxPayload {
		return dst, ErrFrameTooLarge
	}
	dst = AppendVarint(dst, f.Type)
	dst = AppendVarint(dst, uint64(len(f.Payload)))
	dst = append(dst, f.Payload...)
	return dst, nil
}

// ReadFrameFrom 从 buf 开头解出一帧，并返回它消耗的字节数。
//
// 出错时的返回值为 (Frame{}, 0, err)。
func ReadFrameFrom(buf []byte) (Frame, int, error) {
	typ, tn, err := ReadVarint(buf)
	if err != nil {
		return Frame{}, 0, err
	}
	if typ < 1 || typ > MaxType {
		return Frame{}, 0, ErrBadType
	}
	n, ln, err := ReadVarint(buf[tn:])
	if err != nil {
		return Frame{}, 0, err
	}
	if n > MaxPayload {
		return Frame{}, 0, ErrFrameTooLarge
	}
	end := tn + ln + int(n)
	if uint64(end) > uint64(len(buf)) {
		return Frame{}, 0, ErrShortBuffer
	}
	return Frame{Type: typ, Payload: buf[tn+ln : end]}, end, nil
}

// readByte 从 r 读一个字节，容忍 (0, nil)。
// started 为 false 时读不到字节（EOF）属于帧还没开始；
// started 为 true 时属于一帧读了一半。
func readByte(r io.Reader, started bool, scratch []byte) (byte, error) {
	for {
		n, err := r.Read(scratch[:1])
		if n == 1 {
			return scratch[0], nil
		}
		if err != nil {
			if err == io.EOF {
				if started {
					err = io.ErrUnexpectedEOF
				}
			}
			return 0, err
		}
	}
}

// readVarintStream 逐字节地从 r 读一个 uvarint。started 表示这一帧
// 是否已经读到过字节，用来区分 io.EOF 与 io.ErrUnexpectedEOF。
func readVarintStream(r io.Reader, started bool, scratch []byte) (uint64, bool, error) {
	var v uint64
	for i := 0; ; i++ {
		if i == maxVarintLen {
			return 0, started, ErrVarintOverflow
		}
		b, err := readByte(r, started, scratch)
		if err != nil {
			return 0, started, err
		}
		started = true
		if b < 0x80 {
			if i == maxVarintLen-1 && b > 1 {
				return 0, started, ErrVarintOverflow
			}
			return v | uint64(b)<<(7*i), started, nil
		}
		v |= uint64(b&0x7f) << (7 * i)
	}
}

// readFrameHeader 从 r 逐字节读出帧头（类型、载荷长度）并按校验顺序
// 检查类型与载荷上限。
func readFrameHeader(r io.Reader, maxPayload uint64, scratch []byte) (uint64, uint64, error) {
	typ, started, err := readVarintStream(r, false, scratch)
	if err != nil {
		return 0, 0, err
	}
	if typ < 1 || typ > MaxType {
		return 0, 0, ErrBadType
	}
	n, _, err := readVarintStream(r, started, scratch)
	if err != nil {
		return 0, 0, err
	}
	if n > maxPayload {
		return 0, 0, ErrFrameTooLarge
	}
	return typ, n, nil
}

// ReadFrame 从 r 里读出一整帧。
func ReadFrame(r io.Reader) (Frame, error) {
	var scratch [1]byte
	typ, n, err := readFrameHeader(r, MaxPayload, scratch[:])
	if err != nil {
		return Frame{}, err
	}
	// 上限已经在 readFrameHeader 里判过，n <= MaxPayload；空载荷不分配。
	var payload []byte
	if n > 0 {
		payload = make([]byte, n)
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return Frame{}, err
		}
	}
	return Frame{Type: typ, Payload: payload}, nil
}

// CopyFrame 从 r 读出一帧，把载荷原样写进 w，返回写出的载荷字节数。
//
// 它与 ReadFrame 共用帧头解析规则，但走的是流式路径：不使用 MaxPayload，
// 而是允许到 MaxStreamPayload；内部临时缓冲不超过 32 KiB，
// 不得按声明长度分配缓冲。语义见 README「对外契约」第 7 条。
func CopyFrame(r io.Reader, w io.Writer) (int64, error) {
	var scratch [1]byte
	_, remaining, err := readFrameHeader(r, MaxStreamPayload, scratch[:])
	if err != nil {
		return 0, err
	}

	var written int64
	var buf []byte
	for remaining > 0 {
		if buf == nil {
			buf = make([]byte, copyBufSize)
		}
		// 每次最多读到本帧还缺的字节数，绝不越读下一帧。
		want := len(buf)
		if uint64(want) > remaining {
			want = int(remaining)
		}
		nr, er := io.ReadFull(r, buf[:want])
		if nr > 0 {
			nw, ew := w.Write(buf[:nr])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if nw != nr {
				return written, io.ErrShortWrite
			}
			remaining -= uint64(nr)
		}
		if er != nil {
			if er == io.EOF || er == io.ErrUnexpectedEOF {
				er = io.ErrUnexpectedEOF
			}
			return written, er
		}
	}
	return written, nil
}
