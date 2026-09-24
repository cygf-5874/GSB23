// Command wirecheck 是 wirekit 的固定验收程序。
//
// ⚠️ 不要修改本文件。它是判定「题目有没有做对」的依据；
// 修改它只会让判定失效，不会让实现变对。
//
// 15 个场景分五组：
//
//	codec  3 个：varint / 帧字节 / 零拷贝
//	stream 4 个：部分读 / EOF 分界 / 不越读 / (0,nil)
//	safety 3 个：长度上限 / 类型范围 / varint 溢出
//	budget 2 个：分配预算（AllocsPerRun）
//	copy   3 个：CopyFrame 的流式转发、有界内存、EOF 与上限
//
// 用法：
//
//	go run ./wirecheck                  # 跑全部场景
//	go run ./wirecheck -only copy       # 只跑一组
//	go run ./wirecheck -list            # 列出场景
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"runtime/pprof"
	"strings"
	"testing"
	"time"

	"wirekit"
)

const (
	childTimeout = 25 * time.Second
	parentGrace  = 10 * time.Second

	// 解析一个声明了 1 TiB 载荷的帧头之后，本进程总分配增量不得超过这个值。
	maxHeaderAllocBytes = 4 << 20

	// CopyFrame 处理一个声明了 1 GiB 载荷的帧头时，总分配增量不得超过这个值。
	maxCopyAllocBytes = 4 << 20

	// 分配预算判据：读一帧的分配次数上界（一次载荷 + 一次固定帧头缓冲），
	// 以及空载荷帧的上界（只有固定帧头缓冲）。
	maxPayloadFrameAllocs = 2.5
	maxEmptyFrameAllocs   = 1.5
	maxZeroAlloc          = 0.5
)

type scenario struct {
	Group string
	Name  string
	Run   func() error
}

var scenarios = []scenario{
	{"codec", "COD1_varint_exact_bytes", varintExact},
	{"codec", "COD2_append_frame_bytes", appendFrameBytes},
	{"codec", "COD3_from_buffer_alias", fromBufferAlias},
	{"stream", "STR1_one_byte_reads", oneByteReads},
	{"stream", "STR2_eof_semantics", eofSemantics},
	{"stream", "STR3_no_overread", noOverread},
	{"stream", "STR4_zero_length_reads", zeroLengthReads},
	{"safety", "SAF1_length_cap", lengthCap},
	{"safety", "SAF2_type_range", typeRange},
	{"safety", "SAF3_varint_overflow", varintOverflow},
	{"budget", "BUG1_readframe_fixed_alloc_per_frame", readFrameFixedAlloc},
	{"budget", "BUG2_append_and_from_buffer_zero_alloc", appendAndFromZeroAlloc},
	{"copy", "COPY1_stream_through_multiframe", copyStreamThroughFrames},
	{"copy", "COPY2_constant_memory_on_huge_declaration", copyConstantMemory},
	{"copy", "COPY3_eof_type_and_cap", copyEOFTypeAndCap},
}

func main() {
	only := flag.String("only", "", "只跑指定组：codec / stream / safety / budget / copy")
	name := flag.String("scenario", "", "内部使用：只跑单个场景")
	list := flag.Bool("list", false, "列出全部场景")
	flag.Parse()

	if *list {
		for _, sc := range scenarios {
			fmt.Printf("%-8s %s\n", sc.Group, sc.Name)
		}
		return
	}
	if *name != "" {
		os.Exit(runChild(*name))
	}
	os.Exit(runParent(*only))
}

func runChild(name string) int {
	var sc *scenario
	for i := range scenarios {
		if scenarios[i].Name == name {
			sc = &scenarios[i]
			break
		}
	}
	if sc == nil {
		fmt.Fprintf(os.Stderr, "未知场景 %q\n", name)
		return 2
	}

	watchdog := time.AfterFunc(childTimeout, func() {
		fmt.Fprintf(os.Stderr, "看门狗：场景 %s 超过 %s 仍未结束，下面是全部 goroutine 栈\n",
			sc.Name, childTimeout)
		_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
		os.Exit(3)
	})
	defer watchdog.Stop()

	start := time.Now()
	if err := sc.Run(); err != nil {
		fmt.Printf("FAIL (%s): %v\n", time.Since(start).Round(time.Millisecond), err)
		return 1
	}
	fmt.Printf("OK (%s)\n", time.Since(start).Round(time.Millisecond))
	return 0
}

func runParent(only string) int {
	exe, err := os.Executable()
	if err != nil {
		fmt.Println("无法定位自身可执行文件:", err)
		return 1
	}

	groups := []string{"codec", "stream", "safety", "budget", "copy"}
	if only != "" {
		found := false
		for _, g := range groups {
			if g == only {
				found = true
			}
		}
		if !found {
			fmt.Printf("未知分组 %q（可选：codec / stream / safety / budget / copy）\n", only)
			return 2
		}
		groups = []string{only}
	}

	total, passed := 0, 0
	for _, g := range groups {
		fmt.Printf("== 组 %s ==\n", g)
		for _, sc := range scenarios {
			if sc.Group != g {
				continue
			}
			total++

			ctx, cancel := context.WithTimeout(context.Background(), childTimeout+parentGrace)
			cmd := exec.CommandContext(ctx, exe, "-scenario", sc.Name)
			cmd.WaitDelay = 5 * time.Second
			out, runErr := cmd.CombinedOutput()
			cancel()

			text := strings.TrimSpace(string(out))
			if runErr == nil {
				passed++
				fmt.Printf("  PASS  %s/%s\n", sc.Group, sc.Name)
				continue
			}
			fmt.Printf("  FAIL  %s/%s  (%v)\n", sc.Group, sc.Name, runErr)
			for _, line := range tailLines(text, 12) {
				fmt.Printf("        | %s\n", line)
			}
		}
	}

	fmt.Println()
	fmt.Printf("结果：通过 %d/%d\n", passed, total)
	if passed != total {
		return 1
	}
	return 0
}

func tailLines(s string, n int) []string {
	if s == "" {
		return []string{"(子进程没有输出)"}
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = append([]string{"…"}, lines[len(lines)-n:]...)
	}
	return lines
}

// appendUvarint 是本文件自带的编码器。构造测试数据时用它，
// 这样 copy / budget 组的用例不依赖被测代码的 AppendVarint。
func appendUvarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

// -------------------------------------------------------------------- codec

func varintExact() error {
	cases := []struct {
		v    uint64
		want []byte
	}{
		{0, []byte{0x00}},
		{1, []byte{0x01}},
		{127, []byte{0x7f}},
		{128, []byte{0x80, 0x01}},
		{300, []byte{0xac, 0x02}},
		{16383, []byte{0xff, 0x7f}},
		{16384, []byte{0x80, 0x80, 0x01}},
		{1 << 32, []byte{0x80, 0x80, 0x80, 0x80, 0x10}},
		{^uint64(0), []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}},
	}

	for _, tc := range cases {
		got := wirekit.AppendVarint(nil, tc.v)
		if !bytes.Equal(got, tc.want) {
			return fmt.Errorf("AppendVarint(%d) = % x，期望 % x", tc.v, got, tc.want)
		}
		v, n, err := wirekit.ReadVarint(tc.want)
		if err != nil {
			return fmt.Errorf("ReadVarint(% x) = %v", tc.want, err)
		}
		if v != tc.v || n != len(tc.want) {
			return fmt.Errorf("ReadVarint(% x) = (%d, %d)，期望 (%d, %d)",
				tc.want, v, n, tc.v, len(tc.want))
		}
	}
	return nil
}

func appendFrameBytes() error {
	got, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 3, Payload: []byte("hello")})
	if err != nil {
		return fmt.Errorf("AppendFrame = %v", err)
	}
	want := []byte{0x03, 0x05, 'h', 'e', 'l', 'l', 'o'}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("AppendFrame 编码 = % x，期望 % x", got, want)
	}

	// 空载荷
	got, err = wirekit.AppendFrame(nil, wirekit.Frame{Type: 1})
	if err != nil {
		return fmt.Errorf("空载荷 AppendFrame = %v", err)
	}
	if !bytes.Equal(got, []byte{0x01, 0x00}) {
		return fmt.Errorf("空载荷编码 = % x，期望 01 00", got)
	}

	// 出错时 dst 不能被改动
	base := []byte("PRE")
	out, err := wirekit.AppendFrame(base, wirekit.Frame{Type: 0})
	if !errors.Is(err, wirekit.ErrBadType) {
		return fmt.Errorf("Type=0 返回 %v，期望 ErrBadType", err)
	}
	if !bytes.Equal(out, base) {
		return fmt.Errorf("出错时返回值被改成了 % x", out)
	}

	big := wirekit.Frame{Type: 1, Payload: make([]byte, wirekit.MaxPayload+1)}
	out, err = wirekit.AppendFrame(base, big)
	if !errors.Is(err, wirekit.ErrFrameTooLarge) {
		return fmt.Errorf("载荷过大返回 %v，期望 ErrFrameTooLarge", err)
	}
	if !bytes.Equal(out, base) {
		return fmt.Errorf("出错时返回值被改成了 % x", out)
	}
	return nil
}

// COD3：ReadFrameFrom 必须零拷贝 —— 返回的 Payload 与传入的 buf 共享底层数组，
// 同时消耗的字节数必须精确。
func fromBufferAlias() error {
	buf, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 2, Payload: []byte("PAYLOAD")})
	if err != nil {
		return fmt.Errorf("AppendFrame = %v", err)
	}
	buf = append(buf, 0xEE, 0xEE)

	f, n, err := wirekit.ReadFrameFrom(buf)
	if err != nil {
		return fmt.Errorf("ReadFrameFrom = %v", err)
	}
	if want := len(buf) - 2; n != want {
		return fmt.Errorf("消耗 %d 字节，期望 %d", n, want)
	}
	if string(f.Payload) != "PAYLOAD" {
		return fmt.Errorf("Payload = %q", f.Payload)
	}

	// 改写 buf：如果 Payload 与它共享底层数组，这里应该能看到变化
	for i := range buf {
		if buf[i] == 'P' || buf[i] == 'A' || buf[i] == 'Y' || buf[i] == 'L' ||
			buf[i] == 'O' || buf[i] == 'D' {
			buf[i] = 'z'
		}
	}
	if string(f.Payload) != "zzzzzzz" {
		return fmt.Errorf("改写 buf 之后 Payload = %q，期望 \"zzzzzzz\"（ReadFrameFrom 必须零拷贝）",
			f.Payload)
	}
	return nil
}

// ------------------------------------------------------------------- stream

// oneByteReader 每次 Read 只给 1 个字节。
type oneByteReader struct{ data []byte }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

// zeroLengthReader 每读 3 次就返回一次 (0, nil)。
type zeroLengthReader struct {
	data []byte
	n    int
}

func (r *zeroLengthReader) Read(p []byte) (int, error) {
	r.n++
	if r.n%3 == 0 {
		return 0, nil
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func buildStream() ([]byte, []wirekit.Frame, error) {
	frames := []wirekit.Frame{
		{Type: 1, Payload: []byte("a")},
		{Type: 7, Payload: bytes.Repeat([]byte("xyz"), 40)},
		{Type: 15, Payload: []byte("zz")},
	}
	var stream []byte
	for _, f := range frames {
		var err error
		stream, err = wirekit.AppendFrame(stream, f)
		if err != nil {
			return nil, nil, err
		}
	}
	return stream, frames, nil
}

func oneByteReads() error {
	stream, frames, err := buildStream()
	if err != nil {
		return fmt.Errorf("构造流: %v", err)
	}

	r := &oneByteReader{data: stream}
	for i, want := range frames {
		got, err := wirekit.ReadFrame(r)
		if err != nil {
			return fmt.Errorf("第 %d 帧: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			return fmt.Errorf("第 %d 帧 = {%d, %q}，期望 {%d, %q}",
				i, got.Type, got.Payload, want.Type, want.Payload)
		}
	}
	if _, err := wirekit.ReadFrame(r); !errors.Is(err, io.EOF) {
		return fmt.Errorf("流读完之后返回 %v，期望 io.EOF", err)
	}
	return nil
}

func eofSemantics() error {
	if _, err := wirekit.ReadFrame(&oneByteReader{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("空流返回 %v，期望 io.EOF", err)
	}

	// 用一个单帧流来构造各种截断
	one, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 3, Payload: []byte("abcd")})
	if err != nil {
		return fmt.Errorf("构造流: %v", err)
	}

	cases := []struct {
		name string
		buf  []byte
	}{
		{"载荷缺一个字节", one[:len(one)-1]},
		{"只有帧头、载荷一个字节都没有", one[:2]},
		{"类型 varint 被截断", []byte{0x80}},
		{"长度 varint 被截断", []byte{0x03, 0x80}},
	}
	for _, tc := range cases {
		if _, err := wirekit.ReadFrame(&oneByteReader{data: tc.buf}); !errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("%s：返回 %v，期望 io.ErrUnexpectedEOF", tc.name, err)
		}
	}

	// 一帧刚好读完、紧接着流就结束时，应当是 io.EOF 而不是 ErrUnexpectedEOF
	r := &oneByteReader{data: one}
	if _, err := wirekit.ReadFrame(r); err != nil {
		return fmt.Errorf("完整帧: %v", err)
	}
	if _, err := wirekit.ReadFrame(r); !errors.Is(err, io.EOF) {
		return fmt.Errorf("读完整帧之后返回 %v，期望 io.EOF", err)
	}
	return nil
}

// STR3：ReadFrame 只能吃掉一帧的字节，一个多余的字节都不许从 r 里读走。
func noOverread() error {
	stream, frames, err := buildStream()
	if err != nil {
		return fmt.Errorf("构造流: %v", err)
	}

	var rest []byte
	for _, f := range frames[1:] {
		rest, err = wirekit.AppendFrame(rest, f)
		if err != nil {
			return fmt.Errorf("构造剩余流: %v", err)
		}
	}
	rest = append(rest, []byte("TAIL")...)

	full := append(append([]byte{}, stream...), []byte("TAIL")...)
	r := bytes.NewReader(full)

	got, err := wirekit.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("第一帧: %v", err)
	}
	if got.Type != frames[0].Type || !bytes.Equal(got.Payload, frames[0].Payload) {
		return fmt.Errorf("第一帧 = {%d, %q}", got.Type, got.Payload)
	}

	left, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("读剩余: %v", err)
	}
	if !bytes.Equal(left, rest) {
		return fmt.Errorf("ReadFrame 越读了：剩余 % x，期望 % x", left, rest)
	}
	return nil
}

func zeroLengthReads() error {
	stream, frames, err := buildStream()
	if err != nil {
		return fmt.Errorf("构造流: %v", err)
	}

	r := &zeroLengthReader{data: stream}
	for i, want := range frames {
		got, err := wirekit.ReadFrame(r)
		if err != nil {
			return fmt.Errorf("第 %d 帧: %v", i, err)
		}
		if got.Type != want.Type || !bytes.Equal(got.Payload, want.Payload) {
			return fmt.Errorf("第 %d 帧 = {%d, %q}", i, got.Type, got.Payload)
		}
	}
	return nil
}

// ------------------------------------------------------------------- safety

// SAF1：帧头声明一个巨大的载荷长度时，必须直接报 ErrFrameTooLarge，
// 不许按那个长度去分配内存。
func lengthCap() error {
	// type=1，length=1<<40
	header := []byte{0x01, 0x80, 0x80, 0x80, 0x80, 0x80, 0x20}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	_, err := wirekit.ReadFrame(bytes.NewReader(header))

	runtime.ReadMemStats(&after)
	if !errors.Is(err, wirekit.ErrFrameTooLarge) {
		return fmt.Errorf("声明 1TiB 载荷时返回 %v，期望 ErrFrameTooLarge", err)
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > maxHeaderAllocBytes {
		return fmt.Errorf("处理这个帧头时分配了 %d 字节，上限 %d", delta, maxHeaderAllocBytes)
	}

	// 缓冲区路径也一样
	if _, _, err := wirekit.ReadFrameFrom(header); !errors.Is(err, wirekit.ErrFrameTooLarge) {
		return fmt.Errorf("ReadFrameFrom 返回 %v，期望 ErrFrameTooLarge", err)
	}
	return nil
}

func typeRange() error {
	for _, typ := range []uint64{0, wirekit.MaxType + 1, 1 << 40} {
		buf := wirekit.AppendVarint(nil, typ)
		buf = wirekit.AppendVarint(buf, 0)

		if _, _, err := wirekit.ReadFrameFrom(buf); !errors.Is(err, wirekit.ErrBadType) {
			return fmt.Errorf("Type=%d 时 ReadFrameFrom 返回 %v，期望 ErrBadType", typ, err)
		}
		if _, err := wirekit.ReadFrame(bytes.NewReader(buf)); !errors.Is(err, wirekit.ErrBadType) {
			return fmt.Errorf("Type=%d 时 ReadFrame 返回 %v，期望 ErrBadType", typ, err)
		}
	}
	return nil
}

func varintOverflow() error {
	cases := []struct {
		name string
		buf  []byte
		want error
	}{
		{"11 字节 varint", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}, wirekit.ErrVarintOverflow},
		{"第 10 字节 > 1", []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x02}, wirekit.ErrVarintOverflow},
		{"缓冲区不够", []byte{0x80, 0x80, 0x80}, wirekit.ErrShortBuffer},
		{"空缓冲区", nil, wirekit.ErrShortBuffer},
	}

	for _, tc := range cases {
		if _, _, err := wirekit.ReadVarint(tc.buf); !errors.Is(err, tc.want) {
			return fmt.Errorf("%s: ReadVarint 返回 %v，期望 %v", tc.name, err, tc.want)
		}
	}
	return nil
}

// ------------------------------------------------------------------- budget

// sliceReader 是可复用的 Reader：AllocsPerRun 里不能每次新建 Reader，
// 否则被测函数再省也测不出 0 分配。
type sliceReader struct{ data []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// BUG1：ReadFrame 读一帧的分配次数必须有上界，而且不随载荷长度增长。
// （读帧头本身至少要用一个堆缓冲，所以「每帧 0 次」不是本场景的判据。）
func readFrameFixedAlloc() error {
	const (
		runs = 200
		bigN = 1 << 20
	)

	small := bytes.Repeat([]byte("x"), 64)
	smallFrame, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 3, Payload: small})
	if err != nil {
		return fmt.Errorf("构造 64B 载荷的帧: %v", err)
	}
	big := bytes.Repeat([]byte("y"), bigN)
	bigFrame, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 3, Payload: big})
	if err != nil {
		return fmt.Errorf("构造 1MiB 载荷的帧: %v", err)
	}
	emptyFrame, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 4})
	if err != nil {
		return fmt.Errorf("构造空载荷帧: %v", err)
	}

	r := &sliceReader{}
	var got wirekit.Frame
	var readErr error

	measure := func(frame []byte) float64 {
		return testing.AllocsPerRun(runs, func() {
			r.data = frame
			got, readErr = wirekit.ReadFrame(r)
		})
	}

	perSmall := measure(smallFrame)
	if readErr != nil {
		return fmt.Errorf("读 64B 载荷的帧: %v", readErr)
	}
	if got.Type != 3 || !bytes.Equal(got.Payload, small) {
		return fmt.Errorf("读 64B 载荷的帧 = {%d, %d 字节}", got.Type, len(got.Payload))
	}

	// 1MiB 载荷：跑得少一点，避免把时间浪费在搬大量内存上。
	perBig := testing.AllocsPerRun(20, func() {
		r.data = bigFrame
		got, readErr = wirekit.ReadFrame(r)
	})
	if readErr != nil {
		return fmt.Errorf("读 1MiB 载荷的帧: %v", readErr)
	}
	if got.Type != 3 || len(got.Payload) != bigN || got.Payload[0] != 'y' || got.Payload[bigN-1] != 'y' {
		return fmt.Errorf("读 1MiB 载荷的帧 = {%d, %d 字节}", got.Type, len(got.Payload))
	}

	perEmpty := measure(emptyFrame)
	if readErr != nil {
		return fmt.Errorf("读空载荷帧: %v", readErr)
	}
	if got.Type != 4 || len(got.Payload) != 0 {
		return fmt.Errorf("空载荷帧 = {%d, %d 字节}", got.Type, len(got.Payload))
	}

	if perSmall > maxPayloadFrameAllocs {
		return fmt.Errorf("读 64B 载荷的帧平均分配 %.2f 次，上限 %.1f", perSmall, maxPayloadFrameAllocs)
	}
	if perBig > maxPayloadFrameAllocs {
		return fmt.Errorf("读 1MiB 载荷的帧平均分配 %.2f 次，上限 %.1f", perBig, maxPayloadFrameAllocs)
	}
	if perBig > perSmall+maxZeroAlloc {
		return fmt.Errorf("分配次数随载荷长度增长：64B 时 %.2f 次，1MiB 时 %.2f 次", perSmall, perBig)
	}
	if perEmpty > maxEmptyFrameAllocs {
		return fmt.Errorf("读空载荷帧平均分配 %.2f 次，上限 %.1f", perEmpty, maxEmptyFrameAllocs)
	}
	return nil
}

// BUG2：dst 容量足够时 AppendFrame 0 次分配；ReadFrameFrom 零拷贝 0 次分配。
func appendAndFromZeroAlloc() error {
	const runs = 200

	payload := bytes.Repeat([]byte("p"), 96)
	f := wirekit.Frame{Type: 5, Payload: payload}
	encoded, err := wirekit.AppendFrame(nil, f)
	if err != nil {
		return fmt.Errorf("AppendFrame: %v", err)
	}

	dst := make([]byte, 0, len(encoded)+256)
	var out []byte
	var appendErr error
	perAppend := testing.AllocsPerRun(runs, func() {
		dst = dst[:0]
		out, appendErr = wirekit.AppendFrame(dst, f)
	})
	if appendErr != nil {
		return fmt.Errorf("AppendFrame: %v", appendErr)
	}
	if !bytes.Equal(out, encoded) {
		return fmt.Errorf("AppendFrame 编码 = % x，期望 % x", out, encoded)
	}
	if perAppend > maxZeroAlloc {
		return fmt.Errorf("dst 容量足够时 AppendFrame 平均分配 %.2f 次，期望 0 次", perAppend)
	}

	buf := append(append([]byte{}, encoded...), 0xEE, 0xEE)
	var got wirekit.Frame
	var n int
	var fromErr error
	perFrom := testing.AllocsPerRun(runs, func() {
		got, n, fromErr = wirekit.ReadFrameFrom(buf)
	})
	if fromErr != nil {
		return fmt.Errorf("ReadFrameFrom: %v", fromErr)
	}
	if n != len(encoded) || got.Type != f.Type || !bytes.Equal(got.Payload, payload) {
		return fmt.Errorf("ReadFrameFrom = {%d, %q}, 消耗 %d 字节", got.Type, got.Payload, n)
	}
	if perFrom > maxZeroAlloc {
		return fmt.Errorf("ReadFrameFrom 平均分配 %.2f 次，期望 0 次", perFrom)
	}
	return nil
}

// --------------------------------------------------------------------- copy

// verifyingWriter 一边收字节一边比对期望载荷，不额外缓存整帧。
type verifyingWriter struct {
	want []byte
	n    int
	err  error
}

func (w *verifyingWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.n+len(p) > len(w.want) {
		w.err = fmt.Errorf("w 收到了多余的 %d 字节", w.n+len(p)-len(w.want))
		return 0, w.err
	}
	if !bytes.Equal(p, w.want[w.n:w.n+len(p)]) {
		w.err = fmt.Errorf("w 在偏移 %d 收到不一致的字节", w.n)
		return 0, w.err
	}
	w.n += len(p)
	return len(p), nil
}

// prefixWriter 只负责计数并检查字节是否都等于 want。
type prefixWriter struct {
	want byte
	n    int
	bad  bool
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b != w.want {
			w.bad = true
		}
	}
	w.n += len(p)
	return len(p), nil
}

const copyPayloadLen = 4 << 20 // 4 MiB，故意超过 MaxPayload（1 MiB）

// COPY1：4 MiB 载荷的帧（超过 MaxPayload）必须能原样转发；
// 后面紧跟的第二帧还必须能读出来（证明不越读）。
func copyStreamThroughFrames() error {
	payload := make([]byte, copyPayloadLen)
	for i := range payload {
		payload[i] = byte(i * 31)
	}

	// 手工拼帧：type=1，length=4MiB，然后是载荷。
	var stream []byte
	stream = appendUvarint(stream, 1)
	stream = appendUvarint(stream, uint64(len(payload)))
	stream = append(stream, payload...)

	// 第二帧用自带编码器拼，读回时用被测的 ReadFrame。
	tail := appendUvarint(nil, 2)
	tail = appendUvarint(tail, 4)
	tail = append(tail, []byte("tail")...)
	stream = append(stream, tail...)

	r := bytes.NewReader(stream)
	w := &verifyingWriter{want: payload}

	n, err := wirekit.CopyFrame(r, w)
	if err != nil {
		return fmt.Errorf("CopyFrame: %v", err)
	}
	if n != int64(len(payload)) {
		return fmt.Errorf("CopyFrame 返回 %d，期望 %d", n, len(payload))
	}
	if w.err != nil {
		return fmt.Errorf("写入 w 时: %v", w.err)
	}
	if w.n != len(payload) {
		return fmt.Errorf("w 收到 %d 字节，期望 %d", w.n, len(payload))
	}

	f, err := wirekit.ReadFrame(r)
	if err != nil {
		return fmt.Errorf("读第二帧: %v（CopyFrame 越读了？）", err)
	}
	if f.Type != 2 || string(f.Payload) != "tail" {
		return fmt.Errorf("第二帧 = {%d, %q}，期望 {2, \"tail\"}", f.Type, f.Payload)
	}
	return nil
}

// COPY2：帧头声明 1<<30 载荷，但流里只有 1 KiB 就结束 ——
// 必须返回 io.ErrUnexpectedEOF，且不许按声明长度分配内存。
func copyConstantMemory() error {
	header := appendUvarint(nil, 1)
	header = appendUvarint(header, 1<<30)
	body := append(header, bytes.Repeat([]byte{'k'}, 1024)...)

	r := bytes.NewReader(body)
	w := &prefixWriter{want: 'k'}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	n, err := wirekit.CopyFrame(r, w)

	runtime.ReadMemStats(&after)

	if !errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("声明 1GiB 载荷但流只给 1KiB 时返回 %v，期望 io.ErrUnexpectedEOF", err)
	}
	if n < 0 || n > int64(w.n) {
		return fmt.Errorf("CopyFrame 返回 %d，但 w 只收到 %d 字节", n, w.n)
	}
	if w.bad {
		return fmt.Errorf("w 收到了不属于载荷的字节")
	}
	if delta := after.TotalAlloc - before.TotalAlloc; delta > maxCopyAllocBytes {
		return fmt.Errorf("处理这个帧头时分配了 %d 字节，上限 %d", delta, maxCopyAllocBytes)
	}
	return nil
}

// COPY3：CopyFrame 的 EOF 分界、类型范围与流式长度上限。
func copyEOFTypeAndCap() error {
	tooBig := appendUvarint(nil, 1)
	tooBig = appendUvarint(tooBig, 1<<30+1)

	cases := []struct {
		name string
		buf  []byte
		want error
	}{
		{"空流", nil, io.EOF},
		{"类型 varint 只读到一半", []byte{0x80}, io.ErrUnexpectedEOF},
		{"帧头截断（长度 varint 缺字节）", []byte{0x01, 0x80}, io.ErrUnexpectedEOF},
		{"载荷没给完", append(appendUvarint(appendUvarint(nil, 1), 8), 'x'), io.ErrUnexpectedEOF},
		{"类型 0", []byte{0x00, 0x00}, wirekit.ErrBadType},
		{"类型 MaxType+1", []byte{0x10, 0x00}, wirekit.ErrBadType},
		{"声明 1<<30 + 1", tooBig, wirekit.ErrFrameTooLarge},
	}

	for _, tc := range cases {
		w := &prefixWriter{want: 'x'}
		_, err := wirekit.CopyFrame(bytes.NewReader(tc.buf), w)
		if !errors.Is(err, tc.want) {
			return fmt.Errorf("%s：CopyFrame 返回 %v，期望 %v", tc.name, err, tc.want)
		}
	}
	return nil
}
