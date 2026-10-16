// sonic-shim: a drop-in replacement for github.com/bytedance/sonic that uses
// encoding/json as its backend. This allows cross-compilation for architectures
// unsupported by the real sonic (e.g. GOARCH=arm, 32-bit ARM).
module github.com/bytedance/sonic

go 1.21

