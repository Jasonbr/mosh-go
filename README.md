# mosh-go

Pure Go mosh. Client and server. No C dependencies, no CGO.

Wire-compatible with the standard C mosh implementation. You can
use the Go client against a C mosh-server, or the Go server with
a C mosh-client, or go pure Go on both ends.

Implements the full mosh wire protocol: AES-128-OCB3 authenticated
encryption, SSP transport with sequencing and retransmission, datagram
fragmentation, and the protobuf state sync layer.

[![Watch the demo](demo.png)](https://www.youtube.com/watch?v=iWo9I2bjZkI)

## Install

```
go install github.com/unixshells/mosh-go/cmd/mosh@latest
go install github.com/unixshells/mosh-go/cmd/mosh-server@latest
go install github.com/unixshells/mosh-go/cmd/mosh-client@latest
```

## Usage

The `mosh` command works like the C version -- SSHs to the host,
starts `mosh-server`, connects over UDP:

```
mosh user@host
mosh -p 2222 user@host            # custom SSH port
mosh -i ~/.ssh/id_ed25519 user@host
```

Or use the server and client separately:

```sh
# On the server:
mosh-server                       # prints MOSH CONNECT <port> <key>
mosh-server -p 60001              # specific port
mosh-server -s /bin/zsh           # specific shell

# On the client:
MOSH_KEY=<key> mosh-client <host> <port>
```

## Library

```go
import mosh "github.com/unixshells/mosh-go"

// Server
srv, _ := mosh.NewServer("/bin/bash", 60000, 60999)
go srv.Serve()
fmt.Println(srv.ConnectLine())

// Client
c, _ := mosh.Dial("192.168.1.5", 60001, "base64key")
c.Resize(80, 24)
c.Send([]byte("ls\n"))
out := c.Recv(time.Second)
```

## Embedding

`ServeRW` runs the server protocol over an `io.ReadWriteCloser`
instead of a PTY. No shell is spawned -- the caller provides the
I/O. This is how latch bridges mosh into its session model:

```go
srv, _ := mosh.NewServer("", 0, 0)
fmt.Println(srv.ConnectLine())
srv.ServeRW(rw, func(cols, rows uint16) {
	// handle resize
})
```

## Compatibility

Tested against the C reference implementation:

| Client | Server | Status |
|--------|--------|--------|
| Go | Go | always tested |
| Go | C mosh-server | tested if installed |
| C mosh-client | Go | tested if installed |

All three pass. Run `go test -race ./...` to verify.

## Latch extensions

The protobuf layer supports optional latch extension fields.
Standard mosh clients and servers ignore them (forward-compatible).

- **Capability negotiation** (TransportInstruction field 8): bitfield
  exchanged on every datagram. Both sides AND their caps to get the
  intersection. No caps = classic mosh mode.
- **Session control** (Host/UserInstruction field 9): list, switch,
  and create latch sessions over the mosh connection.

## WASM build

The mosh client compiles to WebAssembly for use in the web terminal:

```
GOOS=js GOARCH=wasm CGO_ENABLED=0 go build -tags osusergo -o mosh.wasm ./cmd/mosh-wasm/
```

The `-tags osusergo` flag is required to avoid `os/user` CGO issues with `GOOS=js`.

## Files

- `server.go` -- server, PTY lifecycle, `ServeRW` bridge mode
- `client.go` -- client, send/recv loops
- `transport.go` -- SSP sequencing, timestamps, RTT, retransmission
- `ocb.go` -- AES-128-OCB3 (RFC 7253)
- `fragment.go` -- datagram fragmentation and reassembly
- `pb.go` -- protobuf encoding (transport, host, user, latch extensions)
- `framebuffer.go` -- terminal state tracking for diff protocol
- `predict.go` -- local echo prediction engine (speculative keystroke rendering)

## Dependencies

`creack/pty`, `golang.org/x/crypto`, and the Go standard library.

## License

Copyright (c) 2026 [Unix Shells](https://unixshells.com). MIT license. See [LICENSE](LICENSE).
