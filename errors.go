package wirekit

import "errors"

var (
	// ErrNotImplemented 表示该函数还是空壳。
	ErrNotImplemented = errors.New("wirekit: not implemented")

	// ErrShortBuffer 表示缓冲区里的字节还不够构成想要的东西。
	// 注意：它与 io.ErrUnexpectedEOF 不同 —— 前者是「手头这段字节不够」，
	// 后者是「流已经到头了，但帧还没读完」。
	ErrShortBuffer = errors.New("wirekit: buffer too short")

	// ErrFrameTooLarge 表示帧声明的载荷长度超过了该路径的上限：
	// AppendFrame / ReadFrameFrom / ReadFrame 是 MaxPayload，
	// CopyFrame 是 MaxStreamPayload。
	ErrFrameTooLarge = errors.New("wirekit: frame too large")

	// ErrBadType 表示帧类型不在 [1, MaxType] 内。
	ErrBadType = errors.New("wirekit: bad frame type")

	// ErrVarintOverflow 表示 varint 超过 64 位。
	ErrVarintOverflow = errors.New("wirekit: varint overflows uint64")
)
