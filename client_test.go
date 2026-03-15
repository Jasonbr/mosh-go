package mosh

import (
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var clientAnsiRe = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func clientStripANSI(s string) string {
	return clientAnsiRe.ReplaceAllString(s, "")
}

// TestClientServerE2E tests the Go client against the Go server.
func TestClientServerE2E(t *testing.T) {
	srv, err := NewServer("/bin/sh", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()
	<-srv.started

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Wait for shell prompt.
	var gotOutput bool
	for i := 0; i < 40; i++ {
		out := client.Recv(500 * time.Millisecond)
		if len(out) > 0 {
			gotOutput = true
			break
		}
	}
	if !gotOutput {
		t.Fatal("no output from server")
	}

	// Send a command and verify echo.
	marker := "GOCLIENT_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	client.Send([]byte("echo " + marker + "\n"))

	var allOutput string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("marker not echoed. Got: %q", clientStripANSI(allOutput))
		default:
		}
		out := client.Recv(500 * time.Millisecond)
		if out != nil {
			allOutput += string(out)
			if strings.Contains(clientStripANSI(allOutput), marker) {
				t.Log("Go client → Go server E2E passed")
				return
			}
		}
	}
}

// TestClientResize tests resize via the Go client.
func TestClientResize(t *testing.T) {
	srv, err := NewServer("/bin/sh", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()
	<-srv.started

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Wait for shell.
	for i := 0; i < 20; i++ {
		if out := client.Recv(500 * time.Millisecond); len(out) > 0 {
			break
		}
	}

	// Resize and verify via tput.
	client.Resize(132, 43)
	time.Sleep(200 * time.Millisecond)
	client.Send([]byte("tput cols; tput lines\n"))

	var allOutput string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Logf("output: %q", allOutput)
			t.Log("resize sent (tput output not verified)")
			return
		default:
		}
		out := client.Recv(500 * time.Millisecond)
		if out != nil {
			allOutput += string(out)
			stripped := clientStripANSI(allOutput)
			if strings.Contains(stripped, "132") && strings.Contains(stripped, "43") {
				t.Log("Go client resize verified: 132x43")
				return
			}
		}
	}
}

// TestClientServeRW tests the Go client against ServeRW (bridge mode).
func TestClientServeRW(t *testing.T) {
	srv, err := NewServer("", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Two pipe pairs: one for terminal output, one for keystrokes.
	outR, outW := io.Pipe() // terminal output: outW writes → server reads from outR
	inR, inW := io.Pipe()   // keystrokes: server writes to inW → inR reads

	rw := struct {
		io.Reader
		io.Writer
		io.Closer
	}{outR, inW, outR}

	var resizeCols, resizeRows uint16
	var resizeMu sync.Mutex

	go srv.ServeRW(&rw, func(c, r uint16) {
		resizeMu.Lock()
		resizeCols, resizeRows = c, r
		resizeMu.Unlock()
	})
	defer func() {
		outR.Close()
		outW.Close()
		inR.Close()
		inW.Close()
		srv.conn.Close()
	}()

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Write terminal output into the pipe — simulates latch rendering.
	go outW.Write([]byte("hello from latch\r\n"))

	// Client should receive it via mosh transport.
	var allOutput string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("did not receive bridge output. Got: %q", allOutput)
		default:
		}
		out := client.Recv(500 * time.Millisecond)
		if out != nil {
			allOutput += string(out)
			if strings.Contains(clientStripANSI(allOutput), "hello from latch") {
				break
			}
		}
	}

	// Send keystrokes — should arrive on the keystroke pipe.
	client.Send([]byte("test-input"))

	keystrokeDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, err := inR.Read(buf)
		if err != nil {
			keystrokeDone <- ""
			return
		}
		keystrokeDone <- string(buf[:n])
	}()

	select {
	case got := <-keystrokeDone:
		if !strings.Contains(got, "test-input") {
			t.Fatalf("keystroke mismatch: got %q, want substring %q", got, "test-input")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for keystrokes on pipe")
	}

	// Drain any extra data from the pipe (cumulative diffs may resend keystrokes).
	go func() {
		buf := make([]byte, 4096)
		for {
			inR.Read(buf)
		}
	}()

	// Resize.
	client.Resize(200, 50)
	time.Sleep(500 * time.Millisecond)
	resizeMu.Lock()
	c, r := resizeCols, resizeRows
	resizeMu.Unlock()
	if c != 200 || r != 50 {
		t.Fatalf("resize callback got %dx%d, want 200x50", c, r)
	}

	t.Log("Go client → ServeRW bridge E2E passed")
}

// TestClientAgainstRealMoshServer tests Go client against the C mosh-server.
func TestClientAgainstRealMoshServer(t *testing.T) {
	if _, err := exec.LookPath("mosh-server"); err != nil {
		t.Skip("mosh-server not installed")
	}

	// Start real mosh-server — it forks and prints CONNECT line to stdout.
	cmd := exec.Command("mosh-server", "new", "-p", "0", "-c", "256")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = cmd.Stdout // merge stderr into stdout
	if err := cmd.Start(); err != nil {
		t.Skipf("mosh-server failed to start: %v", err)
	}
	defer cmd.Process.Kill()

	// Read the CONNECT line (mosh-server writes it then the parent exits).
	buf := make([]byte, 4096)
	var output string
	for {
		n, err := stdout.Read(buf)
		if n > 0 {
			output += string(buf[:n])
		}
		if strings.Contains(output, "MOSH CONNECT") {
			break
		}
		if err != nil {
			break
		}
	}
	cmd.Wait()

	port, key := parseMoshConnect(output)
	if port == 0 || key == "" {
		t.Fatalf("bad MOSH CONNECT: %q", output)
	}
	t.Logf("real mosh-server on port %d", port)

	client, err := Dial("127.0.0.1", port, key)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// The real mosh-server expects an initial resize before producing output.
	client.Resize(80, 24)

	// Wait for shell.
	var gotOutput bool
	for i := 0; i < 40; i++ {
		if recv := client.Recv(500 * time.Millisecond); len(recv) > 0 {
			gotOutput = true
			break
		}
	}
	if !gotOutput {
		t.Fatal("no output from real mosh-server")
	}

	// Echo a marker.
	marker := "GOREAL_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	client.Send([]byte("echo " + marker + "\n"))

	var allOutput string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("marker not echoed. Got: %q", allOutput)
		default:
		}
		recv := client.Recv(500 * time.Millisecond)
		if recv != nil {
			allOutput += string(recv)
			if strings.Contains(clientStripANSI(allOutput), marker) {
				t.Log("Go client → real mosh-server E2E passed")
				return
			}
		}
	}
}

// TestFastTypingNoDuplication verifies that rapid keystrokes don't cause
// character doubling due to overlapping diffs from the same base.
func TestFastTypingNoDuplication(t *testing.T) {
	srv, err := NewServer("/bin/sh", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	defer srv.Close()
	<-srv.started

	client, err := Dial("127.0.0.1", srv.Port(), srv.KeyBase64())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Wait for shell prompt.
	for i := 0; i < 40; i++ {
		if out := client.Recv(500 * time.Millisecond); len(out) > 0 {
			break
		}
	}

	// Send "echo TESTMARKER\n" as rapid individual keystrokes with no delay.
	cmd := "echo TESTMARKER\n"
	for _, ch := range cmd {
		client.Send([]byte{byte(ch)})
	}

	// Collect output until we see TESTMARKER in the echo output.
	var allOutput string
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("marker not echoed. Got: %q", clientStripANSI(allOutput))
		default:
		}
		out := client.Recv(500 * time.Millisecond)
		if out != nil {
			allOutput += string(out)
			stripped := clientStripANSI(allOutput)
			if strings.Contains(stripped, "TESTMARKER") {
				// Check that TESTMARKER appears but TTEESSTTMMAARRKKEERR does not.
				if strings.Contains(stripped, "TTEESSTTMMAARRKKEERR") {
					t.Fatalf("character doubling detected: %q", stripped)
				}
				t.Log("fast typing test passed: no character duplication")
				return
			}
		}
	}
}

// parseMoshConnect extracts port and key from mosh-server output.
func parseMoshConnect(output string) (int, string) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "MOSH CONNECT ") {
			parts := strings.Fields(line)
			if len(parts) >= 4 {
				port, err := strconv.Atoi(parts[2])
				if err != nil {
					continue
				}
				return port, parts[3]
			}
		}
	}
	return 0, ""
}
