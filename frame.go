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
//
// 当前 frame.go 里 6 个函数都是空壳，需要把它们实现出来。
package wirekit

import (
	"encoding/binary"
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
	return binary.AppendUvarint(dst, v)
}

// ReadVarint 从 buf 的开头读出一个 uvarint，返回它消耗的字节数。
//
// 出错时的返回值为 (0, 0, err)。
func ReadVarint(buf []byte) (uint64, int, error) {
	v, n := binary.Uvarint(buf)
	if n == 0 {
		return 0, 0, ErrShortBuffer
	}
	if n < 0 {
		return 0, 0, ErrVarintOverflow
	}
	return v, n, nil
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
	dst = binary.AppendUvarint(dst, f.Type)
	dst = binary.AppendUvarint(dst, uint64(len(f.Payload)))
	return append(dst, f.Payload...), nil
}

// ReadFrameFrom 从 buf 开头解出一帧，并返回它消耗的字节数。
//
// 出错时的返回值为 (Frame{}, 0, err)。
func ReadFrameFrom(buf []byte) (Frame, int, error) {
	typ, n, err := ReadVarint(buf)
	if err != nil {
		return Frame{}, 0, err
	}
	if typ < 1 || typ > MaxType {
		return Frame{}, 0, ErrBadType
	}
	off := n
	length, n, err := ReadVarint(buf[off:])
	if err != nil {
		return Frame{}, 0, err
	}
	off += n
	if length > MaxPayload {
		return Frame{}, 0, ErrFrameTooLarge
	}
	if uint64(len(buf)-off) < length {
		return Frame{}, 0, ErrShortBuffer
	}
	return Frame{Type: typ, Payload: buf[off : off+int(length)]}, off + int(length), nil
}

// ReadFrame 从 r 里读出一整帧。
func ReadFrame(r io.Reader) (Frame, error) {
	var b [1]byte
	c, err := readByte(r, &b)
	if err != nil {
		return Frame{}, err
	}
	typ, err := readStreamVarint(r, c, &b)
	if err != nil {
		return Frame{}, err
	}
	if typ < 1 || typ > MaxType {
		return Frame{}, ErrBadType
	}
	c, err = readByte(r, &b)
	if err != nil {
		if err == io.EOF {
			return Frame{}, io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	length, err := readStreamVarint(r, c, &b)
	if err != nil {
		return Frame{}, err
	}
	if length > MaxPayload {
		return Frame{}, ErrFrameTooLarge
	}
	payload := make([]byte, length)
	if length > 0 {
		if _, err := io.ReadFull(r, payload); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return Frame{}, io.ErrUnexpectedEOF
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
	var b [1]byte
	c, err := readByte(r, &b)
	if err != nil {
		return 0, err
	}
	typ, err := readStreamVarint(r, c, &b)
	if err != nil {
		return 0, err
	}
	if typ < 1 || typ > MaxType {
		return 0, ErrBadType
	}
	c, err = readByte(r, &b)
	if err != nil {
		if err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	length, err := readStreamVarint(r, c, &b)
	if err != nil {
		return 0, err
	}
	if length > MaxStreamPayload {
		return 0, ErrFrameTooLarge
	}
	var buf [32 << 10]byte
	remaining := length
	var written int64
	for remaining > 0 {
		chunk := uint64(len(buf))
		if remaining < chunk {
			chunk = remaining
		}
		if _, err := io.ReadFull(r, buf[:chunk]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return written, io.ErrUnexpectedEOF
			}
			return written, err
		}
		n, err := w.Write(buf[:chunk])
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != int(chunk) {
			return written, io.ErrShortWrite
		}
		remaining -= chunk
	}
	return written, nil
}

// readByte 从 r 里读出恰好一个字节。容忍 (0, nil)：继续读。
// buf 是调用方的栈上暂存，避免每次读都分配。
func readByte(r io.Reader, buf *[1]byte) (byte, error) {
	for {
		n, err := r.Read(buf[:])
		if n > 0 {
			return buf[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// readStreamVarint 在已经读出首字节 first 的前提下，继续从 r 里读完一个
// uvarint。流中途结束（帧没读完）报 io.ErrUnexpectedEOF。
func readStreamVarint(r io.Reader, first byte, buf *[1]byte) (uint64, error) {
	v := uint64(first & 0x7f)
	if first < 0x80 {
		return v, nil
	}
	for i := 1; ; i++ {
		c, err := readByte(r, buf)
		if err != nil {
			if err == io.EOF {
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if i == 9 && c > 1 {
			return 0, ErrVarintOverflow
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, nil
		}
	}
}
