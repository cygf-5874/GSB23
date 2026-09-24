# wirekit

一个流式二进制帧编解码器（Go，仅标准库）。

```
+-----------+------------+------------------+
| varint    | varint     | 载荷             |
| 帧类型     | 载荷长度    | 载荷长度个字节     |
+-----------+------------+------------------+
```

两个 varint 都是 uvarint（每字节 7 位、低位在前、最高位表示还有没有后续字节），
与 `encoding/binary` 的 `AppendUvarint` / `Uvarint` 编码一致。

```go
stream, err := wirekit.AppendFrame(nil, wirekit.Frame{Type: 3, Payload: payload})
...
for {
    f, err := wirekit.ReadFrame(conn)
    if err == io.EOF {
        break
    }
    if err != nil {
        return err
    }
    handle(f)
}
```

## 怎么跑

```bash
go test ./...             # 既有用例
go run ./wirecheck        # 验收场景（固定件，15 个）
go run ./wirecheck -list
go run ./wirecheck -only copy
```

## 常量

- `MaxType = 15`：帧类型取值 `[1, 15]`，下限是 1。
- `MaxPayload = 1 << 20`：内存里那一帧的载荷上限，
  适用于 `AppendFrame` / `ReadFrameFrom` / `ReadFrame`。
- `MaxStreamPayload = 1 << 30`：`CopyFrame` 允许的载荷上限，比 `MaxPayload` 大得多。

## 对外契约

以下 9 条同时成立，一条都不能少。

### 1. varint 的精确字节

`AppendVarint(dst []byte, v uint64) []byte` 把 `v` 的 uvarint 追加到 `dst` 末尾
并返回扩展后的切片：`AppendVarint(nil, 0)` 是 `[0x00]`，
`AppendVarint(nil, ^uint64(0))` 是 10 个字节。

`ReadVarint(buf []byte) (uint64, int, error)` 从 `buf` 开头解一个 uvarint，
第二个返回值是它消耗的字节数；出错时返回 `(0, 0, err)`：

- 字节不够构成一个完整 varint → `ErrShortBuffer`
- varint 超过 64 位（多于 10 个字节，或第 10 个字节大于 `0x01`）→ `ErrVarintOverflow`

### 2. 一帧的精确字节与校验顺序

`AppendFrame(dst []byte, f Frame) ([]byte, error)` 依次追加「类型 varint、长度 varint、载荷」。
`Frame{Type: 3, Payload: []byte("hello")}` 的编码是 `03 05 68 65 6c 6c 6f`。校验顺序是：

1. `f.Type` 不在 `[1, MaxType]` → `ErrBadType`
2. `len(f.Payload) > MaxPayload` → `ErrFrameTooLarge`

### 3. `ReadFrameFrom` 的零拷贝与精确消耗字节数

`ReadFrameFrom(buf []byte) (Frame, int, error)` 从 `buf` 开头解一帧，
第二个返回值是消耗的字节数（后面还有别的数据时，只能吃掉这一帧）。
出错时返回 `(Frame{}, 0, err)`：

- 帧头不完整 → `ErrShortBuffer`
- 类型越界 → `ErrBadType`
- 长度超过 `MaxPayload` → `ErrFrameTooLarge`
- 声明的长度超过 `buf` 里剩下的字节 → `ErrShortBuffer`

**零拷贝**：返回的 `Frame.Payload` 与传入的 `buf` 共享底层数组
（它就是 `buf` 的一段子切片，改写 `buf` 能看见，反之亦然）。
调用方要长期保留就自己拷一份。

### 4. `ReadFrame` 的流式容忍

`ReadFrame(r io.Reader) (Frame, error)` 从 `r` 里读出一整帧，返回的 `Payload` 是新分配的。

- **容忍部分读**：`r` 一次只给 1 个字节也要能正常工作。
- **容忍 `(0, nil)`**：`Read` 返回「什么都没读、也没出错」时不许卡死，要继续读。

### 5. `ReadFrame` 的 EOF 分界与不越读

- **EOF 语义**：一帧的第一个字节都没读到就遇到流结束 → `io.EOF`；
  已经读了这一帧的若干字节、帧却没读完 → `io.ErrUnexpectedEOF`。
  一帧刚好读完、紧接着流结束，下一次 `ReadFrame` 才返回 `io.EOF`。
- **不越读**：只从 `r` 里吃掉这一帧的字节，**一个多余的字节都不许读走** ——
  调用方读完一帧后要继续直接从同一个 `r` 读下一帧。

### 6. 类型与长度上限（两套上限）

- 类型不在 `[1, MaxType]` → `ErrBadType`（`AppendFrame` / `ReadFrameFrom` /
  `ReadFrame` / `CopyFrame` 都适用）。
- `AppendFrame` / `ReadFrameFrom` / `ReadFrame` 用 `MaxPayload = 1 << 20`，
  超过 → `ErrFrameTooLarge`。
- **只有 `CopyFrame` 允许更长的载荷**，上限是 `MaxStreamPayload = 1 << 30`，
  超过 → `ErrFrameTooLarge`。
- 上限判定必须在读载荷**之前**完成：声明一个超大长度时直接报 `ErrFrameTooLarge`，
  不许先按那个长度去要内存。

### 7. `CopyFrame` 的有界内存转发

`CopyFrame(r io.Reader, w io.Writer) (int64, error)` 从 `r` 读出一帧，
把载荷**原样**写进 `w`，返回写出的载荷字节数。

- 帧头解析规则与 `ReadFrame` 完全一致（类型不在 `[1, MaxType]` → `ErrBadType`）。
- 上限用 `MaxStreamPayload`（见第 6 条）。
- **内存占用必须与声明长度无关**：内部临时缓冲不超过 **32 KiB**，
  实现**不得**按声明的长度去分配缓冲。
- **不越读**：只从 `r` 消费这一帧的字节；后面还有下一帧时必须能继续读。
- **EOF 分界**（与第 5 条同规则）：第一个字节都没读到 → `io.EOF`；
  读了若干字节却不够一帧 → `io.ErrUnexpectedEOF`。
- **载荷中途不足**：已经写给 `w` 的字节**不回滚**，但必须返回 `io.ErrUnexpectedEOF`。
- `w` 写失败 → 原样返回该错误。
- 返回值是**实际写出的载荷字节数**。

### 8. 分配预算

用 `testing.AllocsPerRun` 测「平均分配次数」，判据是阈值而不是具体机器：

- `AppendFrame` 在 `dst` **容量足够**时，分配次数必须是 **0**。
- `ReadFrameFrom` 零拷贝，分配次数必须是 **0**。
- `ReadFrame` 读一帧的分配次数必须**与载荷长度无关**：64 B 载荷与 1 MiB 载荷的帧，
  平均分配次数必须相同，并且都 **不超过 2**（一次留给载荷本身，一次留给帧头解析的固定缓冲）。
- 载荷长度为 0 的那一帧 **不超过 1**。

不许用 `unsafe` 绕开这条。

### 9. 出错时的不变量

- `AppendFrame` 出错时 `dst` 保持不变（返回值等于传进来的 `dst`）。
- `ReadVarint` / `ReadFrameFrom` 出错时返回 `(0, 0, err)` / `(Frame{}, 0, err)`。
- `CopyFrame` 已经在 `w` 上写出去的字节不回滚。
- 所有错误都要能被 `errors.Is` 匹配到对应的哨兵错误
  （`ErrShortBuffer` / `ErrBadType` / `ErrFrameTooLarge` / `ErrVarintOverflow`，
  或 `io.EOF` / `io.ErrUnexpectedEOF`）。

## 验收

`wirecheck/` 是固定验收程序，**不要修改**。15 个场景分五组：

| 组 | 场景 | 覆盖 |
| --- | --- | --- |
| `codec` | `COD1_varint_exact_bytes` | 9 个边界值的精确字节（契约 1） |
| `codec` | `COD2_append_frame_bytes` | 帧的精确字节、空载荷、出错不改 dst（契约 2、9） |
| `codec` | `COD3_from_buffer_alias` | 零拷贝与精确消耗字节数（契约 3） |
| `stream` | `STR1_one_byte_reads` | 一次只给 1 字节的 Reader（契约 4） |
| `stream` | `STR2_eof_semantics` | `io.EOF` 与 `io.ErrUnexpectedEOF` 的分界（契约 5） |
| `stream` | `STR3_no_overread` | 不许从 `r` 多读一个字节（契约 5） |
| `stream` | `STR4_zero_length_reads` | `(0, nil)` 返回（契约 4） |
| `safety` | `SAF1_length_cap` | 声明 1 TiB 载荷时不许按那个长度分配内存（契约 6） |
| `safety` | `SAF2_type_range` | 类型越界（契约 6） |
| `safety` | `SAF3_varint_overflow` | varint 溢出与缺字节（契约 1） |
| `budget` | `BUG1_readframe_fixed_alloc_per_frame` | `ReadFrame` 每帧分配次数有上界且不随载荷增长（契约 8） |
| `budget` | `BUG2_append_and_from_buffer_zero_alloc` | `AppendFrame` / `ReadFrameFrom` 0 次分配（契约 8） |
| `copy` | `COPY1_stream_through_multiframe` | 4 MiB 载荷（超过 `MaxPayload`）原样转发 + 不越读（契约 6、7） |
| `copy` | `COPY2_constant_memory_on_huge_declaration` | 声明 `1 << 30` 却只有 1 KiB 时内存增量 < 4 MiB（契约 7） |
| `copy` | `COPY3_eof_type_and_cap` | `io.EOF` / `io.ErrUnexpectedEOF` / `ErrBadType` / `ErrFrameTooLarge`（契约 6、7） |

`-only codec|stream|safety|budget|copy` 可以只跑一组（`--only` 等价）。
每个场景在独立子进程里执行，挂死不会遮蔽其余场景；子进程有 25 秒看门狗，
超时会转储全部 goroutine 栈并以退出码 3 结束。

## 目录

```
.
├── go.mod               module wirekit
├── errors.go            ErrShortBuffer / ErrFrameTooLarge / ErrBadType / ErrVarintOverflow
├── frame.go             常量、Frame、六个待实现的函数
├── frame_test.go        既有用例
├── wirecheck/main.go    固定验收程序（勿改）
└── README.md
```

> 现状：`frame.go` 里的 6 个函数全是空壳 —— `AppendVarint` 原样返回 `dst`，
> 其余五个直接返回 `ErrNotImplemented`。既有用例当前全部失败。
