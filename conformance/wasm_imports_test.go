//go:build !wasip1

package conformance_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/universaltill/universal-till/sdk/plugin/channel"
)

// TestBuiltModuleImportsOnlyTheChannelModule audits the SHIPPED binary, not
// the source: a channel guest may import the host module "channel" (exactly
// the SDK's functions) and the WASI shim the Go runtime needs, nothing else
// (contract section 3.1). It needs scripts/build.sh to have run; CI builds
// first and sets CI so a missing module fails there instead of skipping.
func TestBuiltModuleImportsOnlyTheChannelModule(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "bin", "plugin.wasm"))
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("bin/plugin.wasm missing in CI: %v", err)
		}
		t.Skip("bin/plugin.wasm not built (run scripts/build.sh)")
	}
	imports, err := wasmImports(raw)
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{}
	for _, fn := range []string{channel.FnHTTP, channel.FnCursorGet, channel.FnCursorSet, channel.FnSubmit, channel.FnOutboxNext, channel.FnOutboxAck, channel.FnLog} {
		allowed[channel.HostModule+"."+fn] = true
	}
	var bad []string
	seenChannel := false
	for _, im := range imports {
		switch im.module {
		case channel.HostModule:
			seenChannel = true
			if !allowed[im.module+"."+im.name] {
				bad = append(bad, im.module+"."+im.name)
			}
		case "wasi_snapshot_preview1":
			// The Go runtime's own shim (clock, random, stdio, exit). The
			// host gives it no files, no sockets and an empty environment.
		default:
			bad = append(bad, im.module+"."+im.name)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("the module imports outside the channel contract: %v", bad)
	}
	if !seenChannel {
		t.Fatal("the module imports no channel function at all")
	}
}

type wasmImport struct{ module, name string }

// wasmImports reads the import section of a core WebAssembly module.
func wasmImports(b []byte) ([]wasmImport, error) {
	if len(b) < 8 || !bytes.Equal(b[:4], []byte("\x00asm")) {
		return nil, errors.New("not a wasm module")
	}
	r := &wreader{b: b, p: 8}
	for r.p < len(b) {
		id := r.byte()
		size := r.leb()
		end := r.p + int(size)
		if r.err != nil || end > len(b) {
			return nil, errors.New("truncated section")
		}
		if id != 2 {
			r.p = end
			continue
		}
		var out []wasmImport
		for n := r.leb(); n > 0; n-- {
			mod, name := r.name(), r.name()
			switch kind := r.byte(); kind {
			case 0: // func: type index
				r.leb()
			case 1: // table: reftype, limits
				r.byte()
				r.limits()
			case 2: // memory
				r.limits()
			case 3: // global: valtype, mutability
				r.byte()
				r.byte()
			case 4: // tag: attribute, type index
				r.byte()
				r.leb()
			default:
				return nil, errors.New("unknown import kind")
			}
			if r.err != nil {
				return nil, r.err
			}
			out = append(out, wasmImport{mod, name})
		}
		return out, nil
	}
	return nil, nil
}

type wreader struct {
	b   []byte
	p   int
	err error
}

func (r *wreader) byte() byte {
	if r.p >= len(r.b) {
		r.err = errors.New("unexpected end")
		return 0
	}
	c := r.b[r.p]
	r.p++
	return c
}

func (r *wreader) leb() uint64 {
	var v uint64
	for shift := uint(0); shift < 64; shift += 7 {
		c := r.byte()
		v |= uint64(c&0x7f) << shift
		if c&0x80 == 0 || r.err != nil {
			break
		}
	}
	return v
}

func (r *wreader) name() string {
	n := int(r.leb())
	if r.err != nil || r.p+n > len(r.b) {
		r.err = errors.New("bad name")
		return ""
	}
	s := string(r.b[r.p : r.p+n])
	r.p += n
	return s
}

func (r *wreader) limits() {
	flag := r.byte()
	r.leb()
	if flag&1 != 0 {
		r.leb()
	}
}
