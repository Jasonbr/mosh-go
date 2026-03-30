package main

import (
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
	"unicode/utf8"

	mosh "github.com/unixshells/mosh-go"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <host> <port> <base64key>\n", os.Args[0])
		os.Exit(1)
	}

	host := os.Args[1]
	port, _ := strconv.Atoi(os.Args[2])
	key, err := base64.StdEncoding.DecodeString(os.Args[3])
	if err != nil {
		// Try raw base64 without padding.
		key, err = base64.RawStdEncoding.DecodeString(os.Args[2+1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad key: %v\n", err)
			os.Exit(1)
		}
	}

	ocb, err := mosh.NewOCB(key)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ocb: %v\n", err)
		os.Exit(1)
	}

	transport := mosh.NewTransport(ocb, false) // client mode

	// Resolve host.
	addrs, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve: %v\n", err)
		os.Exit(1)
	}

	conn, err := net.DialUDP("udp", nil, addrs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	flush := func() {
		datagrams := transport.Tick()
		for _, dg := range datagrams {
			conn.Write(dg)
		}
	}

	// Read incoming in background.
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			diff := transport.Recv(buf[:n])
			if diff == nil || len(diff) == 0 {
				continue
			}

			instrs, err := mosh.UnmarshalHostMessage(diff)
			if err != nil {
				fmt.Printf("unmarshal error: %v\n", err)
				continue
			}
			for _, hi := range instrs {
				if len(hi.Hoststring) > 0 {
					text := string(hi.Hoststring)
					if !utf8.ValidString(text) {
						text = fmt.Sprintf("(%d raw bytes)", len(hi.Hoststring))
					}
					fmt.Printf("=== hoststring (%d bytes) ===\n%s\n=== end ===\n", len(hi.Hoststring), text)
				}
				if hi.Width > 0 || hi.Height > 0 {
					fmt.Printf("=== resize: %dx%d ===\n", hi.Width, hi.Height)
				}
			}
		}
	}()

	// Tick timer.
	ticker := time.NewTicker(20 * time.Millisecond)
	go func() {
		for range ticker.C {
			flush()
		}
	}()

	// Initial keepalive.
	fmt.Println("Sending initial keepalive...")
	transport.ForceNextSend()
	flush()

	// Resize.
	fmt.Println("Sending resize 80x24...")
	resize := mosh.MarshalUserMessage([]mosh.UserInstruction{
		{Width: 80, Height: 24},
	})
	transport.SetPending(resize)
	flush()

	// Wait for initial output.
	fmt.Println("Waiting 5 seconds for initial output...")
	time.Sleep(5 * time.Second)

	// Send ls + Enter.
	fmt.Println("\nSending ls + Enter...")
	keystroke := mosh.MarshalUserMessage([]mosh.UserInstruction{
		{Keys: []byte("ls\r")},
	})
	transport.SetPending(keystroke)
	flush()

	// Wait for response.
	time.Sleep(3 * time.Second)

	fmt.Println("\nDone.")
	ticker.Stop()
}
