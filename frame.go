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

import "io"

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
	// 未实现
	return dst
}

// ReadVarint 从 buf 的开头读出一个 uvarint，返回它消耗的字节数。
//
// 出错时的返回值为 (0, 0, err)。
func ReadVarint(buf []byte) (uint64, int, error) {
	return 0, 0, ErrNotImplemented
}

// AppendFrame 把一帧编码后追加到 dst 末尾。
//
// 出错时 dst 保持不变。
func AppendFrame(dst []byte, f Frame) ([]byte, error) {
	return dst, ErrNotImplemented
}

// ReadFrameFrom 从 buf 开头解出一帧，并返回它消耗的字节数。
//
// 出错时的返回值为 (Frame{}, 0, err)。
func ReadFrameFrom(buf []byte) (Frame, int, error) {
	return Frame{}, 0, ErrNotImplemented
}

// ReadFrame 从 r 里读出一整帧。
func ReadFrame(r io.Reader) (Frame, error) {
	return Frame{}, ErrNotImplemented
}

// CopyFrame 从 r 读出一帧，把载荷原样写进 w，返回写出的载荷字节数。
//
// 它与 ReadFrame 共用帧头解析规则，但走的是流式路径：不使用 MaxPayload，
// 而是允许到 MaxStreamPayload；内部临时缓冲不超过 32 KiB，
// 不得按声明长度分配缓冲。语义见 README「对外契约」第 7 条。
func CopyFrame(r io.Reader, w io.Writer) (int64, error) {
	return 0, ErrNotImplemented
}
